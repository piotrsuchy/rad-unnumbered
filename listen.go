package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mdlayher/ndp"
	ll "github.com/sirupsen/logrus"
)

// Engine is the main object collecting all running taps
type Engine struct {
	tap  map[int]Tap
	lock sync.RWMutex
}

// NewEngine just setups up a empty new engine
func NewEngine() *Engine {
	return &Engine{
		tap:  make(map[int]Tap),
		lock: sync.RWMutex{},
	}
}

// Add adds a new Interface to be handled by the engine
func (e *Engine) Add(ifIdx int) {
	t, err := NewTap(ifIdx)
	if err != nil {
		ll.Errorf("failed adding ifIndex %d: %s", ifIdx, err)
	}

	e.lock.Lock()
	e.tap[ifIdx] = *t
	e.lock.Unlock()

	go func() {
		if err := t.Listen(); err != nil {
			// Context cancel means a signal was sent, so no need to log an error.
			if err == context.Canceled {
				ll.Infof("%s closed", t.Ifi.Name)
			} else {
				ll.Errorf("%s failed with %s", t.Ifi.Name, err)
			}
			e.lock.Lock()
			delete(e.tap, ifIdx)
			e.lock.Unlock()
		}
	}()

}

// Get returns a lookedup Tap interface thread safe
func (e *Engine) Get(ifIdx int) Tap {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.tap[ifIdx]
}

// Check verifies (thread safe) if tap  is already handled or not
func (e *Engine) Check(ifIdx int) bool {
	e.lock.RLock()
	_, exists := e.tap[ifIdx]
	e.lock.RUnlock()
	return exists
}

// Close stops handling a Tap interfaces and drops it from the map - thread safe
func (e *Engine) Close(ifIdx int) {
	e.tap[ifIdx].Cancel()
	e.lock.Lock()
	delete(e.tap, ifIdx)
	e.lock.Unlock()
}

// Tap is the interface object
type Tap struct {
	c       *ndp.Conn
	Ifi     *net.Interface
	ctx     context.Context
	Cancel  context.CancelFunc
	Prefix  net.IP
	IPs     []*net.IPNet
	Subnets []*net.IPNet
	rs      chan struct{}
}

// NewTap finds, verifies and gets all aparms for a new Tap and returns the object
func NewTap(idx int) (*Tap, error) {

	ifi, err := net.InterfaceByIndex(idx)
	if err != nil {
		return nil, fmt.Errorf("unable to get interface: %v", err)
	}

	hostRoutes, subnets, err := getHostRoutesIpv6(ifi.Index)
	if err != nil {
		return nil, fmt.Errorf("failed getting routes for if %v: %v", ifi.Name, err)
	}

	ll.Debugf("host routes found on %v: %v", ifi.Name, hostRoutes)
	ll.Debugf("subnet routes found on %v: %v", ifi.Name, subnets)

	if hostRoutes == nil && subnets == nil {
		return nil, fmt.Errorf(
			"neither host nor subnet routes to this tap. this may be a private vlan interface, ignoring comletely",
		)
	}

	var prefixChosen net.IP
	if hostRoutes == nil {
		ll.WithFields(ll.Fields{"Interface": ifi.Name}).
			Warnf("%s has no host routes, only advertising RA without prefix for SLAAC", ifi.Name)
	} else {
		// setting a /64 prefix since thats what I need for the SLAAC advertisements
		prefixMask := net.CIDRMask(64, 128)
		// just picking the first in the available list (and setting bits 65-128 to 0)
		prefixChosen = hostRoutes[0].IP.Mask(prefixMask)
	}

	ll.WithFields(ll.Fields{"Interface": ifi.Name}).Infof("%s found: %v", ifi.Name, prefixChosen)

	ctx, cancel := context.WithCancel(context.Background())

	return &Tap{
		ctx:     ctx,
		Cancel:  cancel,
		Ifi:     ifi,
		Prefix:  prefixChosen,
		IPs:     hostRoutes,
		Subnets: subnets,
	}, nil
}

// Listen starts listening for RS on this tap and sends periodic RAs
func (t Tap) Listen() error {
	var c *ndp.Conn
	var ip net.IP
	var err error

	// need this hacky loop since there are occasions where the OS seems to lock the tap for about 15sec (or sometimes longer)
	// on innitial creation. causing the dialer to fail.
	// this loop checks the context for cancellation but otherwise continues to re-try
	for {
		c, ip, err = ndp.Listen(t.Ifi, ndp.LinkLocal)
		if err != nil {
			ll.Warnf("unable to dial linklocal: %v, retrying...", err)
			time.Sleep(1 * time.Second)
			// Was the context canceled already?
			select {
			case <-t.ctx.Done():
				return context.Canceled
				//fmt.Errorf("got stopped by %v while still dialing %v", t.ctx.Err(), err)
			default:
			}
		} else {
			ll.Debugf("successfully dialed linklocal: %v", t.Ifi.Name)
			break
		}
	}
	defer c.Close()

	ll.WithFields(ll.Fields{"Interface": t.Ifi.Name}).
		Debugf("handling interface: %s, mac: %s, ip: %s", t.Ifi.Name, t.Ifi.HardwareAddr, ip)

	return doRA(t.ctx, c, t.Ifi.HardwareAddr, t.Prefix)
}
