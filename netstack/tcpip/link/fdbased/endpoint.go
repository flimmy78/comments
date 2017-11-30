// Copyright 2016 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fdbased provides the implemention of data-link layer endpoints
// backed by boundary-preserving file descriptors (e.g., TUN devices,
// seqpacket/datagram sockets).
//
// FD based endpoints can be used in the networking stack by calling New() to
// create a new endpoint, and then passing it as an argument to
// Stack.CreateNIC().
package fdbased

import (
	"syscall"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/header"
	"github.com/google/netstack/tcpip/link/rawfile"
	"github.com/google/netstack/tcpip/stack"
)

// BufConfig defines the shape of the vectorised view used to read packets from the NIC.
var BufConfig = []int{128, 256, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}

type endpoint struct {
	// fd is the file descriptor used to send and receive packets.
	fd int

	// mtu (maximum transmission unit) is the maximum size of a packet.
	mtu uint32

	// closed is a function to be called when the FD's peer (if any) closes
	// its end of the communication pipe.
	closed func(*tcpip.Error)

	// vv用views来初始化，代表实际接收到的数据包
	vv     *buffer.VectorisedView
	iovecs []syscall.Iovec
	views  []buffer.View
}

// New creates a new fd-based endpoint.
func New(fd int, mtu uint32, closed func(*tcpip.Error)) tcpip.LinkEndpointID {
	syscall.SetNonblock(fd, true)

	e := &endpoint{
		fd:     fd,
		mtu:    mtu,
		closed: closed,
		// buffer.View是[]byte的别名类型
		views:  make([]buffer.View, len(BufConfig)),
		iovecs: make([]syscall.Iovec, len(BufConfig)),
	}
	// 根据已经分配的views创建vectorisedView并设置它的大小
	vv := buffer.NewVectorisedView(0, e.views)
	e.vv = &vv
	// RegisterLinkEndpoint将e加入linkEndpoints这个哈希表中
	return stack.RegisterLinkEndpoint(e)
}

// Attach launches the goroutine that reads packets from the file descriptor and
// dispatches them via the provided dispatcher.
func (e *endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	go e.dispatchLoop(dispatcher)
}

// MTU implements stack.LinkEndpoint.MTU. It returns the value initialized
// during construction.
func (e *endpoint) MTU() uint32 {
	return e.mtu
}

// MaxHeaderLength returns the maximum size of the header. Given that it
// doesn't have a header, it just returns 0.
func (*endpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link address of this endpoint.
func (*endpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// WritePacket writes outbound packets to the file descriptor. If it is not
// currently writable, the packet is dropped.
// WritePacket将包写往文件描述符，如果当前不能写，则将该包丢弃
func (e *endpoint) WritePacket(_ *stack.Route, hdr *buffer.Prependable, payload buffer.View, protocol tcpip.NetworkProtocolNumber) *tcpip.Error {
	if payload == nil {
		return rawfile.NonBlockingWrite(e.fd, hdr.UsedBytes())

	}

	return rawfile.NonBlockingWrite2(e.fd, hdr.UsedBytes(), payload)
}

func (e *endpoint) capViews(n int, buffers []int) int {
	c := 0
	for i, s := range buffers {
		c += s
		if c >= n {
			// 将最后一个views多出部分的cap去除
			e.views[i].CapLength(s - (c - n))
			// 返回已经使用的views数目
			return i + 1
		}
	}
	return len(buffers)
}

func (e *endpoint) allocateViews(bufConfig []int) {
	for i, v := range e.views {
		if v != nil {
			break
		}
		b := buffer.NewView(bufConfig[i])
		// 为每个views分配内存
		e.views[i] = b
		// e.iovecs[]中的元素为syscall.Iovec
		e.iovecs[i] = syscall.Iovec{
			// 用分配获得的view初始化Iovec
			Base: &b[0],
			Len:  uint64(len(b)),
		}
	}
}

// dispatch reads one packet from the file descriptor and dispatches it.
// dispatch从文件描述符中读出一个包再进行转发
// largerV貌似并没有用到？
func (e *endpoint) dispatch(d stack.NetworkDispatcher, largeV buffer.View) (bool, *tcpip.Error) {
	// 为endpoint中的每个view和iovec根据BufConfig分配内存
	e.allocateViews(BufConfig)

	// 将endpoint的iovecs[]传递给BlockingReadv，返回的n为读到的字节数
	n, err := rawfile.BlockingReadv(e.fd, e.iovecs)
	if err != nil {
		return false, err
	}

	if n <= 0 {
		return false, nil
	}

	// used为使用了的view的数量
	used := e.capViews(n, BufConfig)
	// 重新设置vv为已使用的views
	e.vv.SetViews(e.views[:used])
	// 设置vv为读取的包的大小
	e.vv.SetSize(n)

	// We don't get any indication of what the packet is, so try to guess
	// if it's an IPv4 or IPv6 packet.
	var p tcpip.NetworkProtocolNumber
	// 从第一个views中读取出封包类型
	switch header.IPVersion(e.views[0]) {
	case header.IPv4Version:
		p = header.IPv4ProtocolNumber
	case header.IPv6Version:
		p = header.IPv6ProtocolNumber
	default:
		return true, nil
	}

	d.DeliverNetworkPacket(e, "", p, e.vv)

	// Prepare e.views for another packet: release used views.
	// 将包传输完成之后，重新将views中的每个view设置为nil
	for i := 0; i < used; i++ {
		e.views[i] = nil
	}

	return true, nil
}

// dispatchLoop reads packets from the file descriptor in a loop and dispatches
// them to the network stack.
// dispatchLoop从文件描述符中读取数据
func (e *endpoint) dispatchLoop(d stack.NetworkDispatcher) *tcpip.Error {
	v := buffer.NewView(header.MaxIPPacketSize)
	for {
		cont, err := e.dispatch(d, v)
		if err != nil || !cont {
			if e.closed != nil {
				e.closed(err)
			}
			return err
		}
	}
}

// InjectableEndpoint is an injectable fd-based endpoint. The endpoint writes
// to the FD, but does not read from it. All reads come from injected packets.
type InjectableEndpoint struct {
	endpoint

	dispatcher stack.NetworkDispatcher
}

// Attach saves the stack network-layer dispatcher for use later when packets
// are injected.
func (e *InjectableEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher = dispatcher
}

// Inject injects an inbound packet.
func (e *InjectableEndpoint) Inject(protocol tcpip.NetworkProtocolNumber, vv *buffer.VectorisedView) {
	uu := vv.Clone(nil)
	e.dispatcher.DeliverNetworkPacket(e, "", protocol, &uu)
}

// NewInjectable creates a new fd-based InjectableEndpoint.
func NewInjectable(fd int, mtu uint32) (tcpip.LinkEndpointID, *InjectableEndpoint) {
	syscall.SetNonblock(fd, true)

	e := &InjectableEndpoint{endpoint: endpoint{
		fd:  fd,
		mtu: mtu,
	}}

	return stack.RegisterLinkEndpoint(e), e
}