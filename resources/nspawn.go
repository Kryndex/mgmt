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

package resources

import (
	"encoding/gob"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/purpleidea/mgmt/event"
	"github.com/purpleidea/mgmt/util"

	systemdUtil "github.com/coreos/go-systemd/util"
	"github.com/godbus/dbus"
	machined "github.com/joejulian/go-systemd/machine1"
	errwrap "github.com/pkg/errors"
	"github.com/purpleidea/mgmt/global"
)

const (
	running           = "running"
	stopped           = "stopped"
	dbusInterface     = "org.freedesktop.machine1.Manager"
	machineNew        = "org.freedesktop.machine1.Manager.MachineNew"
	machineRemoved    = "org.freedesktop.machine1.Manager.MachineRemoved"
	nspawnServiceTmpl = "systemd-nspawn@%s"
)

func init() {
	gob.Register(&NspawnRes{})
}

// NspawnRes is an nspawn container resource
type NspawnRes struct {
	BaseRes `yaml:",inline"`
	State   string `yaml:"state"`
	// we're using the svc resource to start the machine because that's
	// what machinectl does. We're not using svc.Watch because then we
	// would have two watches potentially racing each other and producing
	// potentially unexpected results. We get everything we need to
	// monitor the machine state changes from the org.freedesktop.machine1 object.
	svc     SvcRes
}

// Init runs some startup code for this resource
func (obj *NspawnRes) Init() error {
	var serviceName = fmt.Sprintf(nspawnServiceTmpl, obj.GetName())
	obj.svc.Name = serviceName
	obj.svc.State = running
	if err := obj.svc.Init(); err != nil {
		return err
	}
	obj.BaseRes.kind = "Nspawn"
	return obj.BaseRes.Init()
}

// NewNspawnRes is the constructor for this resource
func NewNspawnRes(name string, state string) (*NspawnRes, error) {
	obj := &NspawnRes{
		BaseRes: BaseRes{
			Name: name,
		},
		State: state,
	}
	return obj, obj.Init()
}

// Validate params
func (obj *NspawnRes) Validate() error {
	validStates := map[string]struct{}{
		stopped: {},
		running: {},
	}
	if _, exists := validStates[obj.State]; exists {
		return fmt.Errorf("Invalid State: %s", obj.State)
	}
	return obj.svc.Validate()
}

// Watch for state changes and sends a message to the bus if there is a change
func (obj *NspawnRes) Watch(processChan chan event.Event) error {
	if obj.IsWatching() {
		return nil
	}
	obj.SetWatching(true)
	defer obj.SetWatching(false)
	cuid := obj.converger.Register()
	defer cuid.Unregister()

	var startup bool
	Startup := func(block bool) <-chan time.Time {
		if block {
			return nil // blocks forever
		}
		// 1/2 the resolution of converged timeout
		return time.After(time.Duration(500) * time.Millisecond)
	}

	// this resource depends on systemd ensure that it's running
	if !systemdUtil.IsRunningSystemd() {
		return fmt.Errorf("Systemd is not running.")
	}

	// create a private message bus
	bus, err := util.SystemBusPrivateUsable()
	if err != nil {
		return errwrap.Wrapf(err, "Failed to connect to bus")
	}

	// add a match rule to match messages going through the message bus
	call := bus.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		fmt.Sprintf("type='signal',interface='%s',eavesdrop='true'",
			dbusInterface))
	// <-call.Done
	if err := call.Err; err != nil {
		return err
	}
	buschan := make(chan *dbus.Signal, 10)
	bus.Signal(buschan)

	var send = false
	var exit = false

	for {
		obj.SetState(ResStateWatching)
		select {
		// if this resource has been idle for long enough, set converged
		// to allow the main loop to act on a converged status
		case <-cuid.ConvergedTimer():
			cuid.SetConverged(true) // converged!
			continue

		case event := <-obj.Events():
			cuid.SetConverged(false)
			// we avoid sending events on unpause
			if exit, send = obj.ReadEvent(&event); exit {
				return nil // exit
			}

		case <-Startup(startup):
			cuid.SetConverged(false)
			send = true
			obj.isStateOK = false

		// process org.freedesktop.machine1 events for this resource's name
		case event := <-buschan:
			if event.Body[0] == obj.GetName() {
				log.Printf("%v[%v]: event received: %#v", obj.Kind(), obj.GetName(), event.Name)
				if event.Name == machineNew {
					log.Printf("%v[%v]: Machine started", obj.Kind(), obj.GetName())
					send = true
				}
				if event.Name == machineRemoved {
					log.Printf("%v[%v]: Machine stopped", obj.Kind(), obj.GetName())
					send = true
				}
			}
		}

		// do all our event sending all together to avoid duplicate msgs
		if send || !obj.isStateOK {
			// TODO: remove this printf
			log.Printf("%v[%v]: object: %#v", obj.Kind(), obj.GetName(), obj)
			startup = true // startup finished
			send = false
			if exit, err := obj.DoSend(processChan, ""); exit || err != nil {
				return err // we exit or bubble up a NACK
			}
		}
	}
}

// CheckApply is run to check the state and, if apply is true, to apply the
// necessary changes to reach the desired state. this is run before Watch and
// again if watch finds a change occurring to the state
func (obj *NspawnRes) CheckApply(apply bool) (checkok bool, err error) {
	if global.DEBUG {
		log.Printf("%v[%v]: CheckApply(%t)", obj.Kind(), obj.GetName(), apply)
	}


	// this resource depends on systemd ensure that it's running
	if !systemdUtil.IsRunningSystemd() {
		return false, errors.New("Systemd is not running.")
	}

	// connect to org.freedesktop.machine1.Manager
	conn, err := machined.New()
	if err != nil {
		return false, fmt.Errorf("Failed to connect to dbus: %s", err)
	}

	// compare the current state with the desired state and perform the
	// appropriate action
	var exists = true
	properties, err := conn.GetProperties(obj.GetName())
	if err != nil {
		if err, ok := err.(dbus.Error); ok && err.Name !=
			"org.freedesktop.machine1.NoSuchMachine" {
			return false, err
		}
		exists = false
		// if we could not successfully get the properties because
		// there's no such machine the machine is stopped
		// error if we need the image ignore if we don't
		if _, err = conn.GetImage(obj.GetName()); err != nil && obj.State != stopped {
			return false, fmt.Errorf(
				"No machine nor image named '%s'",
				obj.GetName())
		}
	}
	log.Printf("%v[%v]: properties: %#v", obj.Kind(), obj.GetName(), properties)

	// if the machine doesn't exist and is supposed to
	// be stopped or the state matches we're done
	if !exists && obj.State == stopped || properties["State"] == obj.State {
		if global.DEBUG {
			log.Printf("%v[%v]: CheckApply() in valid state", obj.Kind(), obj.GetName())
		}
		obj.isStateOK = true // state is validated
		return true, nil
	}

	// end of state checking. if we're here, checkok is false
	if !apply {
		obj.isStateOK = true
		return false, nil
	}

	obj.isStateOK = false // state is dirty

	if global.DEBUG {
		log.Printf("%v[%v]: CheckApply() applying '%s' state", obj.Kind(), obj.GetName(), obj.State)
	}

	if obj.State == running {
		// start the machine using svc resource
		log.Printf("%v[%v]: Starting machine", obj.Kind(), obj.GetName())
		return obj.svc.CheckApply(apply)
	}
	if obj.State == stopped {
		// terminate the machine with
		// org.freedesktop.machine1.Manager.KillMachine
		log.Printf("%v[%v]: Stopping machine", obj.Kind(), obj.GetName())
		if err := conn.KillMachine(obj.GetName()); err != nil {
			errwrap.Wrap(err, "Failed to stop machine")
			return false, err
		}
	}
	return false,nil
}

// NspawnUID is a unique resource identifier
type NspawnUID struct {
	// NOTE: there is also a name variable in the BaseUID struct, this is
	// information about where this UID came from, and is unrelated to the
	// information about the resource we're matching. That data which is
	// used in the IFF function, is what you see in the struct fields here
	BaseUID
	name string // the machine name
}

// IFF aka if and only if they are equivalent, return true. If not, false
func (obj *NspawnUID) IFF(uid ResUID) bool {
	res, ok := uid.(*NspawnUID)
	if !ok {
		return false
	}
	return obj.name == res.name
}

// GetUIDs includes all params to make a unique identification of this object
// most resources only return one although some resources can return multiple
func (obj *NspawnRes) GetUIDs() []ResUID {
	x := &NspawnUID{
		BaseUID: BaseUID{name: obj.GetName(), kind: obj.Kind()},
		name:    obj.Name, // svc name
	}
	return append([]ResUID{x}, obj.svc.GetUIDs()...)
}

// GroupCmp returns whether two resources can be grouped together or not
func (obj *NspawnRes) GroupCmp(r Res) bool {
	_, ok := r.(*NspawnRes)
	if !ok {
		return false
	}
	// TODO: depending on if the systemd service api allows batching we
	// might be able to build this, although not sure how useful it is
	// it might just eliminate parallelism be bunching up the graph
	return false // not possible atm
}

// Compare two resources and return if they are equivalent
func (obj *NspawnRes) Compare(res Res) bool {
	switch res.(type) {
	case *NspawnRes:
		res := res.(*NspawnRes)
		if !obj.BaseRes.Compare(res) {
			return false
		}
		if obj.Name != res.Name {
			return false
		}
		if !obj.svc.Compare(&res.svc) {
			return false
		}
	default:
		return false
	}
	return true
}

// AutoEdges returns the AutoEdge interface in this case no autoedges are used
func (obj *NspawnRes) AutoEdges() AutoEdge {
	return nil
}