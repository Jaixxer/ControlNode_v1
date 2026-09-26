// Package cluster holds the timing contract shared by the control plane and the
// worker nodes.
//
// Both sides must agree on HeartbeatInterval: the worker sends on that cadence
// and the control plane uses it to decide when a silent worker is gone. Keeping
// it here means the two can never drift apart.
package cluster

import "time"

// HeartbeatInterval is how often a worker reports in while it is connected.
// Workers keep sending during a docker build, so a build never looks like a
// dead worker.
const HeartbeatInterval = 10 * time.Second

// OfflineAfter is how long the control plane waits before declaring a silent
// worker offline: three missed heartbeats.
const OfflineAfter = 3 * HeartbeatInterval

// SweepInterval is how often the control plane looks for silent workers. It is
// short relative to OfflineAfter so a worker is marked offline promptly once the
// window passes.
const SweepInterval = HeartbeatInterval

// Worker status values stored in the database.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)
