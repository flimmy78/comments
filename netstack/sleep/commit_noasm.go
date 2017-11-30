// Copyright 2017 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !race
// +build !amd64

package sleep

import "sync/atomic"

// commitSleep signals to wakers that the given g is now sleeping. Wakers can
// then fetch it and wake it.
// commitSleep用来通知waker，给定的g正处于休眠状态。Waker可以fetch并且wake它们
//
// The commit may fail if wakers have been asserted after our last check, in
// which case they will have set s.waitingG to zero.
// 如果waker在我们上一次check之后asserted，它们会将s.waitingG设置为0
//
// It is written in assembly because it is called from g0, so it doesn't have
// a race context.
func commitSleep(g uintptr, waitingG *uintptr) bool {
	for {
		// Check if the wait was aborted.
		if atomic.LoadUintptr(waitingG) == 0 {
			return false
		}

		// Try to store the G so that wakers know who to wake.
		if atomic.CompareAndSwapUintptr(waitingG, preparingG, g) {
			return true
		}
	}
}
