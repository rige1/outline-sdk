// Copyright 2023 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const CloseTimeout = 5 * time.Second

type CopyResult struct {
	Written int64
	Err     error
}

func CopyDecorator(dst io.Writer, src io.Reader) <- chan CopyResult {
	resultCh := make(chan CopyResult)
	go func() {
		written, err := io.Copy(dst, src)
		resultCh <- CopyResult{Written: written, Err: err }
	}()
	return resultCh
}

func (app App) Run() error {

	forceQuit := make(chan struct{})
	// this WaitGroup must Wait() after tun is closed
	trafficCopyWg := &sync.WaitGroup{}
	defer trafficCopyWg.Wait()

	tun, err := newTunDevice(app.RoutingConfig.TunDeviceName, app.RoutingConfig.TunDeviceIP)
	if err != nil {
		return fmt.Errorf("failed to create tun device: %w", err)
	}

	// disable IPv6 before resolving Shadowsocks server IP
	prevIPv6, err := enableIPv6(false)
	if err != nil {
		return fmt.Errorf("failed to disable IPv6: %w", err)
	}
	defer enableIPv6(prevIPv6)

	ss, err := NewOutlineDevice(*app.TransportConfig)
	if err != nil {
		return fmt.Errorf("failed to create OutlineDevice: %w", err)
	}

	ss.Refresh()


	defer closeDevices(tun, ss, forceQuit)
	// Copy the traffic from tun device to OutlineDevice bidirectionally
	trafficCopyWg.Add(2)
	go func() {
		resultCh := CopyDecorator(ss, tun)
		select {
		case res := <- resultCh:
			logging.Info.Printf("tun -> OutlineDevice stopped: %v %v\n", res.Written, res.Err)
			trafficCopyWg.Done()	
		case <-forceQuit:
			logging.Info.Printf("tun -> OutlineDevice stopped: forceQuit")
			trafficCopyWg.Done()
		}
	}()
	go func() {
		resultCh := CopyDecorator(tun, ss)
		select {
		case res := <- resultCh:
			logging.Info.Printf("OutlineDevice -> tun stopped: %v %v\n", res.Written, res.Err)
			trafficCopyWg.Done()	
		case <- forceQuit:
			logging.Info.Printf("OutlineDevice -> tun stopped: forceQuit")
			trafficCopyWg.Done()
		}
	}()

	err = setSystemDNSServer(app.RoutingConfig.DNSServerIP)
	if err != nil {
		return fmt.Errorf("failed to configure system DNS: %w", err)
	}
	defer restoreSystemDNSServer()

	if err := startRouting(ss.GetServerIP().String(), app.RoutingConfig); err != nil {
		return fmt.Errorf("failed to configure routing: %w", err)
	}
	defer stopRouting(app.RoutingConfig.RoutingTableID)


	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, unix.SIGTERM, unix.SIGHUP)
	s := <-sigc
	logging.Info.Printf("received %v, terminating...\n", s)
	return nil
}

func closeDevices(tun io.Closer, outlineDevice io.Closer, forceQuit chan struct{}) {
	tunClosedWithoutTimeout := closeWithTimeout("tun", tun)
	outlineDeviceClosedWithoutTimeout := closeWithTimeout("OutlineDevice", outlineDevice)

	if !tunClosedWithoutTimeout {
		forceQuit <- struct{}{}
	}
	if !outlineDeviceClosedWithoutTimeout {
		forceQuit <- struct{}{}
	}
	close(forceQuit)
}

func closeWithTimeout(deviceName string, closer io.Closer) bool {
	ch := make(chan error)
	go func() {
		ch <- closer.Close()
	}()

	select {
	case err := <-ch:
		logging.Info.Printf("device %v closed with result: %v", deviceName, err)
		return true
	case <-time.After(CloseTimeout):
		logging.Err.Printf("device %v close timeout", deviceName)
		return false
	}
}
