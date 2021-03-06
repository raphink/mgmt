// Mgmt
// Copyright (C) 2013-2016+ James Shubin and the project contributors
// Written by James Shubin <james@shubin.ca> and the project contributors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"log"
	"sync"
	"time"
)

// Converger is the general interface for implementing a convergence watcher
type Converger interface { // TODO: need a better name
	Register() ConvergerUUID
	IsConverged(ConvergerUUID) bool   // is the UUID converged ?
	SetConverged(ConvergerUUID, bool) // set the converged state of the UUID
	Unregister(ConvergerUUID)
	Start()
	Pause()
	Loop(bool)
	ConvergedTimer(ConvergerUUID) <-chan time.Time
}

// ConvergerUUID is the interface resources can use to notify with if converged
// you'll need to use part of the Converger interface to Register initially too
type ConvergerUUID interface {
	ID() uint64    // get Id
	IsValid() bool // has Id been initialized ?
	InvalidateID() // set Id to nil
	IsConverged() bool
	SetConverged(bool)
	Unregister()
	ConvergedTimer() <-chan time.Time
}

// converger is an implementation of the Converger interface
type converger struct {
	timeout int           // must be zero (instant) or greater seconds to run
	exitFn  func()        // TODO: generalize functionality eventually?
	channel chan struct{} // signal here to run an isConverged check
	control chan bool     // control channel for start/pause
	mutex   sync.RWMutex  // used for controlling access to status and lastid
	lastid  uint64
	status  map[uint64]bool
}

// convergerUUID is an implementation of the ConvergerUUID interface
type convergerUUID struct {
	converger Converger
	id        uint64
}

// NewConverger builds a new converger struct
func NewConverger(timeout int, exitFn func()) *converger {
	return &converger{
		timeout: timeout,
		exitFn:  exitFn,
		channel: make(chan struct{}),
		control: make(chan bool),
		lastid:  0,
		status:  make(map[uint64]bool),
	}
}

// Register assigns a ConvergerUUID to the caller
func (obj *converger) Register() ConvergerUUID {
	obj.mutex.Lock()
	defer obj.mutex.Unlock()
	obj.lastid++
	obj.status[obj.lastid] = false // initialize as not converged
	return &convergerUUID{
		converger: obj,
		id:        obj.lastid,
	}
}

// IsConverged gets the converged status of a uuid
func (obj *converger) IsConverged(uuid ConvergerUUID) bool {
	if !uuid.IsValid() {
		log.Fatal("Id of ConvergerUUID is nil!")
	}
	obj.mutex.RLock()
	isConverged, found := obj.status[uuid.ID()] // lookup
	obj.mutex.RUnlock()
	if !found {
		log.Fatal("Id of ConvergerUUID is unregistered!")
	}
	return isConverged
}

// SetConverged updates the converger with the converged state of the UUID
func (obj *converger) SetConverged(uuid ConvergerUUID, isConverged bool) {
	if !uuid.IsValid() {
		log.Fatal("Id of ConvergerUUID is nil!")
	}
	obj.mutex.Lock()
	if _, found := obj.status[uuid.ID()]; !found {
		log.Fatal("Id of ConvergerUUID is unregistered!")
	}
	obj.status[uuid.ID()] = isConverged // set
	obj.mutex.Unlock()                  // unlock *before* poke or deadlock!
	if isConverged {                    // only poke if it would be helpful
		// run in a go routine so that we never block... just queue up!
		// this allows us to send events, even if we haven't started...
		go func() { obj.channel <- struct{}{} }()
	}
}

// isConverged returns true if *every* registered uuid has converged
func (obj *converger) isConverged() bool {
	obj.mutex.RLock() // take a read lock
	defer obj.mutex.RUnlock()
	for _, v := range obj.status {
		if !v { // everyone must be converged for this to be true
			return false
		}
	}
	return true
}

// Unregister dissociates the ConvergedUUID from the converged checking
func (obj *converger) Unregister(uuid ConvergerUUID) {
	if !uuid.IsValid() {
		log.Fatal("Id of ConvergerUUID is nil!")
	}
	obj.mutex.Lock()
	delete(obj.status, uuid.ID())
	obj.mutex.Unlock()
	uuid.InvalidateID()
}

// Start causes a Converger object to start or resume running
func (obj *converger) Start() {
	obj.control <- true
}

// Pause causes a Converger object to stop running temporarily
func (obj *converger) Pause() { // FIXME: add a sync ACK on pause before return
	obj.control <- false
}

// Loop is the main loop for a Converger object; it usually runs in a goroutine
// TODO: we could eventually have each resource tell us as soon as it converges
// and then keep track of the time delays here, to avoid callers needing select
// NOTE: when we have very short timeouts, if we start before all the resources
// have joined the map, then it might appears as if we converged before we did!
func (obj *converger) Loop(startPaused bool) {
	if obj.control == nil {
		log.Fatal("Converger not initialized correctly")
	}
	if startPaused { // start paused without racing
		select {
		case e := <-obj.control:
			if !e {
				log.Fatal("Converger expected true!")
			}
		}
	}
	for {
		select {
		case e := <-obj.control: // expecting "false" which means pause!
			if e {
				log.Fatal("Converger expected false!")
			}
			// now i'm paused...
			select {
			case e := <-obj.control:
				if !e {
					log.Fatal("Converger expected true!")
				}
				// restart
				// kick once to refresh the check...
				go func() { obj.channel <- struct{}{} }()
				continue
			}

		case _ = <-obj.channel:
			if !obj.isConverged() {
				continue
			}
			// we have converged!

			if obj.timeout >= 0 { // only run if timeout is valid
				if obj.exitFn != nil {
					// call an arbitrary function
					obj.exitFn()
				}
			}

			for { // unblock/drain
				<-obj.channel
			}
			//return
			// TODO: would it be useful to loop and wait again ?
			// we would need to reset or wait otherwise we'd be
			// likely instantly already converged when we looped!
		}
	}
}

// ConvergedTimer adds a timeout to a select call and blocks until then
// TODO: this means we could eventually have per resource converged timeouts
func (obj *converger) ConvergedTimer(uuid ConvergerUUID) <-chan time.Time {
	// be clever: if i'm already converged, this timeout should block which
	// avoids unnecessary new signals being sent! this avoids fast loops if
	// we have a low timeout, or in particular a timeout == 0
	if uuid.IsConverged() {
		// blocks the case statement in select forever!
		return TimeAfterOrBlock(-1)
	}
	return TimeAfterOrBlock(obj.timeout)
}

// Id returns the unique id of this UUID object
func (obj *convergerUUID) ID() uint64 {
	return obj.id
}

// IsValid tells us if the id is valid or has already been destroyed
func (obj *convergerUUID) IsValid() bool {
	return obj.id != 0 // an id of 0 is invalid
}

// InvalidateID marks the id as no longer valid
func (obj *convergerUUID) InvalidateID() {
	obj.id = 0 // an id of 0 is invalid
}

// IsConverged is a helper function to the regular IsConverged method
func (obj *convergerUUID) IsConverged() bool {
	return obj.converger.IsConverged(obj)
}

// SetConverged is a helper function to the regular SetConverged notification
func (obj *convergerUUID) SetConverged(isConverged bool) {
	obj.converger.SetConverged(obj, isConverged)
}

// Unregister is a helper function to unregister myself
func (obj *convergerUUID) Unregister() {
	obj.converger.Unregister(obj)
}

// ConvergedTimer is a helper around the regular ConvergedTimer method
func (obj *convergerUUID) ConvergedTimer() <-chan time.Time {
	return obj.converger.ConvergedTimer(obj)
}
