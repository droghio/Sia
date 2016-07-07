package host

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

// rpcSettingsDeprecated is a specifier for a deprecated settings request.
var rpcSettingsDeprecated = types.Specifier{'S', 'e', 't', 't', 'i', 'n', 'g', 's'}

// threadedUpdateHostname periodically runs 'managedLearnHostname', which
// checks if the host's hostname has changed, and makes an updated host
// announcement if so.
func (h *Host) threadedUpdateHostname() {
	err := h.tg.AddPermanent()
	if err != nil {
		return
	}
	defer h.tg.DonePermanent()

	for {
		h.managedLearnHostname()
		// Wait 30 minutes to check again. If the hostname is changing
		// regularly (more than once a week), we want the host to be able to be
		// seen as having 95% uptime. Every minute that the announcement is
		// pointing to the wrong address is a minute of perceived downtime to
		// the renters.
		select {
		case <-h.tg.StopChan():
			return
		case <-time.After(time.Minute * 30):
			continue
		}
	}
}

// initNetworking performs actions like port forwarding, and gets the host
// established on the network.
func (h *Host) initNetworking(address string) (err error) {
	// Create listener and set address.
	h.listener, err = h.dependencies.listen("tcp", address)
	if err != nil {
		return err
	}
	// Automatically close the listener when h.tg.Stop() is called.
	h.tg.BeforeStop(func() {
		err := h.listener.Close()
		if err != nil {
			h.log.Println("WARN: closing the listener failed:", err)
		}
	})

	// Set the port.
	_, port, err := net.SplitHostPort(h.listener.Addr().String())
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.port = port
	if build.Release == "testing" {
		// Set the autoAddress to localhost for testing builds only.
		h.autoAddress = modules.NetAddress(net.JoinHostPort("localhost", h.port))
	}
	h.mu.Unlock()

	// Non-blocking, perform port forwarding and hostname discovery.
	go func() {
		err := h.tg.Add()
		if err != nil {
			return
		}
		defer h.tg.Done()

		err = h.managedForwardPort()
		if err != nil {
			h.log.Println("ERROR: failed to forward port:", err)
		}
		go h.threadedUpdateHostname()
	}()

	// Launch the listener.
	go h.threadedListen()
	return nil
}

// threadedHandleConn handles an incoming connection to the host, typically an
// RPC.
func (h *Host) threadedHandleConn(conn net.Conn) {
	// Close the conn on host.Close or when the method terminates, whichever comes
	// first.
	connCloseChan := make(chan struct{})
	defer close(connCloseChan)
	go func() {
		select {
		case <-h.tg.StopChan():
		case <-connCloseChan:
		}
		conn.Close()
	}()

	err := h.tg.Add()
	if err != nil {
		return
	}
	defer h.tg.Done()

	// Set an initial duration that is generous, but finite. RPCs can extend
	// this if desired.
	err = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if err != nil {
		h.log.Println("WARN: could not set deadline on connection:", err)
		return
	}

	// Read a specifier indicating which action is being called.
	var id types.Specifier
	if err := encoding.ReadObject(conn, &id, 16); err != nil {
		atomic.AddUint64(&h.atomicUnrecognizedCalls, 1)
		h.log.Debugf("WARN: incoming conn %v was malformed: %v", conn.RemoteAddr(), err)
		return
	}

	switch id {
	case modules.RPCDownload:
		atomic.AddUint64(&h.atomicDownloadCalls, 1)
		err = h.managedRPCDownload(conn)
	case modules.RPCRenewContract:
		atomic.AddUint64(&h.atomicRenewCalls, 1)
		err = h.managedRPCRenewContract(conn)
	case modules.RPCFormContract:
		atomic.AddUint64(&h.atomicFormContractCalls, 1)
		err = h.managedRPCFormContract(conn)
	case modules.RPCReviseContract:
		atomic.AddUint64(&h.atomicReviseCalls, 1)
		err = h.managedRPCReviseContract(conn)
	case modules.RPCRecentRevision:
		atomic.AddUint64(&h.atomicRecentRevisionCalls, 1)
		_, _, err = h.managedRPCRecentRevision(conn)
	case modules.RPCSettings:
		atomic.AddUint64(&h.atomicSettingsCalls, 1)
		err = h.managedRPCSettings(conn)
	case rpcSettingsDeprecated:
		h.log.Debugln("Received deprecated settings call")
	default:
		h.log.Debugf("WARN: incoming conn %v requested unknown RPC \"%v\"", conn.RemoteAddr(), id)
		atomic.AddUint64(&h.atomicUnrecognizedCalls, 1)
	}
	if err != nil {
		atomic.AddUint64(&h.atomicErroredCalls, 1)

		// If there have been less than 1000 errored rpcs, print the error
		// message. This is to help developers debug live systems that are
		// running into issues. Ultimately though, this error can be triggered
		// by a malicious actor, and therefore should not be logged except for
		// DEBUG builds.
		erroredCalls := atomic.LoadUint64(&h.atomicErroredCalls)
		if erroredCalls < 1e3 {
			h.log.Printf("WARN: incoming RPC \"%v\" failed: %v", id, err)
		} else {
			h.log.Debugf("WARN: incoming RPC \"%v\" failed: %v", id, err)
		}
	}
}

// listen listens for incoming RPCs and spawns an appropriate handler for each.
func (h *Host) threadedListen() {
	err := h.tg.AddPermanent()
	if err != nil {
		return
	}
	defer h.tg.DonePermanent()

	// Receive connections until an error is returned by the listener. When an
	// error is returned, there will be no more calls to receive.
	for {
		// Block until there is a connection to handle.
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}

		go h.threadedHandleConn(conn)
	}
}

// NetAddress returns the address at which the host can be reached.
func (h *Host) NetAddress() modules.NetAddress {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.settings.NetAddress != "" {
		return h.settings.NetAddress
	}
	return h.autoAddress
}

// NetworkMetrics returns information about the types of rpc calls that have
// been made to the host.
func (h *Host) NetworkMetrics() modules.HostNetworkMetrics {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return modules.HostNetworkMetrics{
		// TODO: Up/Down bandwidth

		DownloadCalls:     atomic.LoadUint64(&h.atomicDownloadCalls),
		ErrorCalls:        atomic.LoadUint64(&h.atomicErroredCalls),
		FormContractCalls: atomic.LoadUint64(&h.atomicFormContractCalls),
		RenewCalls:        atomic.LoadUint64(&h.atomicRenewCalls),
		ReviseCalls:       atomic.LoadUint64(&h.atomicReviseCalls),
		SettingsCalls:     atomic.LoadUint64(&h.atomicSettingsCalls),
		UnrecognizedCalls: atomic.LoadUint64(&h.atomicUnrecognizedCalls),
	}
}
