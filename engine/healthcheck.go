// Copyright 2012 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Author: angusc@google.com (Angus Cameron)

package engine

// This file contains structs and functions to manage communications with the
// healthcheck component.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/google/seesaw/common/seesaw"
	"github.com/google/seesaw/engine/config"
	"github.com/google/seesaw/healthcheck"
	"github.com/google/seesaw/ipvs"
	ncclient "github.com/google/seesaw/ncc/client"

	log "github.com/golang/glog"
)

const (
	channelSize int = 1000

	dsrMarkBase = 1 << 16
	dsrMarkSize = 16000
)

// checkerKey is the unique key of the health checker.
type checkerKey struct {
	key CheckKey
	cfg config.Healthcheck
}

// markKey is the unique key of the marks
type markKey struct {
	backend seesaw.IP
	mode    seesaw.HealthcheckMode
}

// healthcheckManager manages the healthcheck configuration for a Seesaw Engine.
type healthcheckManager struct {
	engine *Engine
	ncc    ncclient.NCC

	markAlloc     *markAllocator
	marks         map[markKey]uint32
	next          healthcheck.Id
	vserverChecks map[string]map[CheckKey]*check // keyed by vserver name

	cfgs    map[healthcheck.Id]*healthcheck.Config
	checks  map[healthcheck.Id][]*check
	ids     map[checkerKey]healthcheck.Id
	enabled bool
	lock    sync.RWMutex // Guards cfgs, checks, enabled and ids.

	quit    chan bool
	stopped chan bool
	vcc     chan vserverChecks
}

// newHealthcheckManager creates a new healthcheckManager.
func newHealthcheckManager(e *Engine) *healthcheckManager {
	return &healthcheckManager{
		engine:        e,
		marks:         make(map[markKey]uint32),
		markAlloc:     newMarkAllocator(dsrMarkBase, dsrMarkSize),
		ncc:           e.ncc,
		next:          healthcheck.Id((uint64(os.Getpid()) & 0xFFFF) << 48),
		vserverChecks: make(map[string]map[CheckKey]*check),
		quit:          make(chan bool),
		stopped:       make(chan bool),
		vcc:           make(chan vserverChecks, 1000),
		enabled:       true,
	}
}

// configs returns the healthcheck Configs for a Seesaw Engine. The returned
// map should only be read, not mutated. If the healthcheckManager is disabled,
// then nil is returned.
func (h *healthcheckManager) configs() map[healthcheck.Id]*healthcheck.Config {
	h.lock.RLock()
	defer h.lock.RUnlock()
	if !h.enabled {
		return nil
	}
	return h.cfgs
}

// update updates the healthchecks for a vserver.
func (h *healthcheckManager) update(vserverName string, checks map[CheckKey]*check) {
	if checks == nil {
		delete(h.vserverChecks, vserverName)
	} else {
		h.vserverChecks[vserverName] = checks
	}
	h.buildMaps()
}

// enable enables the healthcheck manager for the Seesaw Engine.
func (h *healthcheckManager) enable() {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.enabled = true
}

// disable disables the healthcheck manager for the Seesaw Engine.
func (h *healthcheckManager) disable() {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.enabled = false
}

// shutdown requests the healthcheck manager to shutdown.
func (h *healthcheckManager) shutdown() {
	h.quit <- true
	<-h.stopped
}

// buildMaps builds the cfgs, checks, and ids maps based on the vserverChecks.
func (h *healthcheckManager) buildMaps() {
	allChecks := make(map[CheckKey]*check)
	for _, vchecks := range h.vserverChecks {
		for k, c := range vchecks {
			if allChecks[k] == nil {
				allChecks[k] = c
			} else {
				log.Warningf("Duplicate key: %v", k)
			}
		}
	}

	h.lock.RLock()
	ids := h.ids
	cfgs := h.cfgs
	h.lock.RUnlock()
	newIDs := make(map[checkerKey]healthcheck.Id)
	newCfgs := make(map[healthcheck.Id]*healthcheck.Config)
	newChecks := make(map[healthcheck.Id][]*check)

	for key, c := range allChecks {
		cKey := checkerKey{
			key: dedup(key),
			cfg: *c.healthcheck,
		}
		id, ok := ids[cKey]
		if !ok {
			id = h.next
			h.next++
		}
		cfg, ok := cfgs[id]
		if !ok {
			newCfg, err := h.newConfig(id, cKey.key, c.healthcheck)
			if err != nil {
				log.Error(err)
				continue
			}
			cfg = newCfg
		}

		newIDs[cKey] = id
		newCfgs[id] = cfg
		newChecks[id] = append(newChecks[id], c)
	}

	h.lock.Lock()
	h.ids = newIDs
	h.cfgs = newCfgs
	h.checks = newChecks
	h.lock.Unlock()

	h.pruneMarks()
}

// dedup removes service related fields in a CheckKey which doesn't affect how a hc work.
// Note that for DSR or TUN typed healthcheck, they are needed.
func dedup(key CheckKey) CheckKey {
	key.Name = ""
	if key.HealthcheckMode == seesaw.HCModePlain {
		key.VserverIP = seesaw.IP{}
		key.ServicePort = 0
		key.ServiceProtocol = 0
	}
	return key
}

// queueHealthState handles Notifications from the healthcheck component.
func (h *healthcheckManager) queueHealthState(n *healthcheck.Notification) error {
	log.V(1).Infof("Received healthcheck notification: %v", n)

	h.lock.RLock()
	enabled := h.enabled
	cfg := h.cfgs[n.Id]
	checkList := h.checks[n.Id]
	h.lock.RUnlock()

	if !enabled {
		log.Warningf("Healthcheck manager is disabled; ignoring healthcheck notification %v", n)
		return nil
	}

	if cfg == nil || len(checkList) == 0 {
		log.Warningf("Unknown healthcheck ID %v", n.Id)
		return nil
	}

	for _, check := range checkList {
		note := &checkNotification{
			key:         check.key,
			description: cfg.Checker.String(),
			status:      n.Status,
		}
		check.vserver.queueCheckNotification(note)
	}

	return nil
}

// SyncHealthCheckNotification stores a status notification for a healthcheck.
type SyncHealthCheckNotification struct {
	Key CheckKey
	healthcheck.Status
}

// String returns the string representation for the given notification.
func (s *SyncHealthCheckNotification) String() string {
	return fmt.Sprintf("%s %v", s.Key, s.State)
}

func (h *healthcheckManager) newConfig(id healthcheck.Id, key CheckKey, hc *config.Healthcheck) (*healthcheck.Config, error) {
	host := key.BackendIP.IP()
	port := int(hc.Port)
	mark := 0

	// For DSR or TUN we use the VIP address as the target and specify a
	// mark for the backend.
	ip := host
	if key.HealthcheckMode != seesaw.HCModePlain {
		ip = key.VserverIP.IP()
		mkey := markKey{
			backend: key.BackendIP,
			mode:    key.HealthcheckMode,
		}
		mark = int(h.markBackend(mkey))
	}

	var checker healthcheck.Checker
	var target *healthcheck.Target
	switch hc.Type {
	case seesaw.HCTypeDNS:
		dns := healthcheck.NewDNSChecker(ip, port)
		target = &dns.Target
		queryType, err := healthcheck.DNSType(hc.Method)
		if err != nil {
			return nil, err
		}
		dns.Answer = hc.Receive
		dns.Question.Name = hc.Send
		dns.Question.Qtype = queryType

		checker = dns
	case seesaw.HCTypeHTTP:
		http := healthcheck.NewHTTPChecker(ip, port)
		target = &http.Target
		if hc.Send != "" {
			http.Request = hc.Send
		}
		if hc.Receive != "" {
			http.Response = hc.Receive
		}
		if hc.Code != 0 {
			http.ResponseCode = hc.Code
		}
		http.Proxy = hc.Proxy
		if hc.Method != "" {
			http.Method = hc.Method
		}
		checker = http
	case seesaw.HCTypeHTTPS:
		https := healthcheck.NewHTTPChecker(ip, port)
		target = &https.Target
		if hc.Send != "" {
			https.Request = hc.Send
		}
		if hc.Receive != "" {
			https.Response = hc.Receive
		}
		if hc.Code != 0 {
			https.ResponseCode = hc.Code
		}
		https.Secure = true
		https.TLSVerify = hc.TLSVerify
		https.Proxy = hc.Proxy
		if hc.Method != "" {
			https.Method = hc.Method
		}
		checker = https
	case seesaw.HCTypeICMP:
		// DSR or TUN cannot be used with ICMP (at least for now).
		if key.HealthcheckMode != seesaw.HCModePlain {
			return nil, errors.New("ICMP healthchecks cannot be used with DSR or TUN mode")
		}
		ping := healthcheck.NewPingChecker(ip)
		target = &ping.Target
		checker = ping
	case seesaw.HCTypeRADIUS:
		radius := healthcheck.NewRADIUSChecker(ip, port)
		target = &radius.Target
		// TODO(jsing): Ugly hack since we do not currently have
		// separate protobuf messages for each healthcheck type...
		send := strings.Split(hc.Send, ":")
		if len(send) != 3 {
			return nil, errors.New("RADIUS healthcheck has invalid send value")
		}
		radius.Username = send[0]
		radius.Password = send[1]
		radius.Secret = send[2]
		if hc.Receive != "" {
			radius.Response = hc.Receive
		}
		checker = radius
	case seesaw.HCTypeTCP:
		tcp := healthcheck.NewTCPChecker(ip, port)
		target = &tcp.Target
		tcp.Send = hc.Send
		tcp.Receive = hc.Receive
		checker = tcp
	case seesaw.HCTypeTCPTLS:
		tcp := healthcheck.NewTCPChecker(ip, port)
		target = &tcp.Target
		tcp.Send = hc.Send
		tcp.Receive = hc.Receive
		tcp.Secure = true
		tcp.TLSVerify = hc.TLSVerify
		checker = tcp
	case seesaw.HCTypeUDP:
		udp := healthcheck.NewUDPChecker(ip, port)
		target = &udp.Target
		udp.Send = hc.Send
		udp.Receive = hc.Receive
		checker = udp
	default:
		return nil, fmt.Errorf("Unknown healthcheck type: %v", hc.Type)
	}

	target.Host = host
	target.Mark = mark
	target.Mode = hc.Mode

	hcc := healthcheck.NewConfig(id, checker)
	hcc.Interval = hc.Interval
	hcc.Timeout = hc.Timeout
	hcc.Retries = hc.Retries

	return hcc, nil
}

// run runs the healthcheck manager and processes incoming vserver checks.
func (h *healthcheckManager) run() {
	for {
		select {
		case <-h.quit:
			h.unmarkAllBackends()
			h.stopped <- true
		case vc := <-h.vcc:
			h.update(vc.vserverName, vc.checks)
		}
	}
}

// expire invalidates the state of all configured healthchecks.
func (h *healthcheckManager) expire() {
	h.lock.RLock()
	ids := h.ids
	h.lock.RUnlock()

	status := healthcheck.Status{State: healthcheck.StateUnknown}
	for _, id := range ids {
		h.queueHealthState(&healthcheck.Notification{Id: id, Status: status})
	}
}

// markBackend returns a mark for the specified key and sets up the IPVS
// service entry if it does not exist.
func (h *healthcheckManager) markBackend(key markKey) uint32 {
	mark, ok := h.marks[key]
	if ok {
		return mark
	}

	mark, err := h.markAlloc.get()
	if err != nil {
		log.Fatalf("Failed to get mark: %v", err)
	}
	h.marks[key] = mark

	ip := net.IPv6zero
	if key.backend.AF() == seesaw.IPv4 {
		ip = net.IPv4zero
	}

	flags := ipvs.DFForwardRoute
	if key.mode == seesaw.HCModeTUN {
		flags = ipvs.DFForwardTunnel
	}

	ipvsSvc := &ipvs.Service{
		Address:      ip,
		Protocol:     ipvs.IPProto(0),
		Port:         0,
		Scheduler:    "rr",
		FirewallMark: mark,
		Destinations: []*ipvs.Destination{
			{
				Address: key.backend.IP(),
				Port:    0,
				Weight:  1,
				Flags:   flags,
			},
		},
	}

	log.Infof("Adding DSR/TUN IPVS service for %s (mark %d)", key.backend, mark)
	if err := h.ncc.IPVSAddService(ipvsSvc); err != nil {
		log.Fatalf("Failed to add IPVS service for DSR/TUN: %v", err)
	}

	return mark
}

// unmarkBackend removes the mark for a given key and removes the IPVS
// service entry if it exists.
func (h *healthcheckManager) unmarkBackend(key markKey) {
	mark, ok := h.marks[key]
	if !ok {
		return
	}

	ip := net.IPv6zero
	if key.backend.AF() == seesaw.IPv4 {
		ip = net.IPv4zero
	}

	flags := ipvs.DFForwardRoute
	if key.mode == seesaw.HCModeTUN {
		flags = ipvs.DFForwardTunnel
	}

	ipvsSvc := &ipvs.Service{
		Address:      ip,
		Protocol:     ipvs.IPProto(0),
		Port:         0,
		Scheduler:    "rr",
		FirewallMark: mark,
		Destinations: []*ipvs.Destination{
			{
				Address: key.backend.IP(),
				Port:    0,
				Weight:  1,
				Flags:   flags,
			},
		},
	}

	log.Infof("Removing DSR/TUN IPVS service for %s (mark %d)", key.backend, mark)
	if err := h.ncc.IPVSDeleteService(ipvsSvc); err != nil {
		log.Fatalf("Failed to remove DSR/TUN IPVS service: %v", err)
	}

	delete(h.marks, key)
	h.markAlloc.put(mark)
}

// pruneMarks unmarks backends that no longer have DSR or TUN healthchecks configured.
func (h *healthcheckManager) pruneMarks() {
	h.lock.RLock()
	checks := h.checks
	h.lock.RUnlock()

	backends := make(map[markKey]bool)
	for _, checkList := range checks {
		for _, check := range checkList {
			if check.key.HealthcheckMode == seesaw.HCModePlain {
				continue
			}
			mkey := markKey{
				backend: check.key.BackendIP,
				mode:    check.key.HealthcheckMode,
			}
			backends[mkey] = true
		}
	}

	for mkey := range h.marks {
		if _, ok := backends[mkey]; !ok {
			h.unmarkBackend(mkey)
		}
	}
}

// unmarkAllBackends unmarks all backends that were previously marked.
func (h *healthcheckManager) unmarkAllBackends() {
	for ip := range h.marks {
		h.unmarkBackend(ip)
	}
}
