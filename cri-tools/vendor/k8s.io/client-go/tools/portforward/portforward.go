/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package portforward

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/runtime"
)

// TODO move to API machinery and re-unify with kubelet/server/portfoward
// The subprotocol "portforward.k8s.io" is used for port forwarding.
const PortForwardProtocolV1Name = "portforward.k8s.io"

// PortForwarder knows how to listen for local connections and forward them to
// a remote pod via an upgraded HTTP request.
// PortForwarder知道如何监听本地连接并将它转发到远程的pod，通过upgraded HTTP request
type PortForwarder struct {
	ports    []ForwardedPort
	stopChan <-chan struct{}

	dialer        httpstream.Dialer
	streamConn    httpstream.Connection
	listeners     []io.Closer
	Ready         chan struct{}
	requestIDLock sync.Mutex
	requestID     int
	out           io.Writer
	errOut        io.Writer
}

// ForwardedPort contains a Local:Remote port pairing.
type ForwardedPort struct {
	Local  uint16
	Remote uint16
}

/*
	valid port specifications:

	5000
	- forwards from localhost:5000 to pod:5000

	8888:5000
	- forwards from localhost:8888 to pod:5000

	0:5000
	:5000
	- selects a random available local port,
	  forwards from localhost:<random port> to pod:5000
*/
func parsePorts(ports []string) ([]ForwardedPort, error) {
	var forwards []ForwardedPort
	for _, portString := range ports {
		parts := strings.Split(portString, ":")
		var localString, remoteString string
		if len(parts) == 1 {
			localString = parts[0]
			remoteString = parts[0]
		} else if len(parts) == 2 {
			localString = parts[0]
			if localString == "" {
				// support :5000
				localString = "0"
			}
			remoteString = parts[1]
		} else {
			return nil, fmt.Errorf("Invalid port format '%s'", portString)
		}

		localPort, err := strconv.ParseUint(localString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("Error parsing local port '%s': %s", localString, err)
		}

		remotePort, err := strconv.ParseUint(remoteString, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("Error parsing remote port '%s': %s", remoteString, err)
		}
		if remotePort == 0 {
			return nil, fmt.Errorf("Remote port must be > 0")
		}

		forwards = append(forwards, ForwardedPort{uint16(localPort), uint16(remotePort)})
	}

	return forwards, nil
}

// New creates a new PortForwarder.
func New(dialer httpstream.Dialer, ports []string, stopChan <-chan struct{}, readyChan chan struct{}, out, errOut io.Writer) (*PortForwarder, error) {
	if len(ports) == 0 {
		return nil, errors.New("You must specify at least 1 port")
	}
	// 解析ports
	parsedPorts, err := parsePorts(ports)
	if err != nil {
		return nil, err
	}
	return &PortForwarder{
		dialer:   dialer,
		ports:    parsedPorts,
		stopChan: stopChan,
		Ready:    readyChan,
		out:      out,
		errOut:   errOut,
	}, nil
}

// ForwardPorts formats and executes a port forwarding request. The connection will remain
// open until stopChan is closed.
// ForwardPorts创建并且执行一个port forwarding request
// 连接会一直打开，直到stopChan被关闭
func (pf *PortForwarder) ForwardPorts() error {
	defer pf.Close()

	var err error
	// 获取stream connection
	pf.streamConn, _, err = pf.dialer.Dial(PortForwardProtocolV1Name)
	if err != nil {
		return fmt.Errorf("error upgrading connection: %s", err)
	}
	defer pf.streamConn.Close()

	return pf.forward()
}

// forward dials the remote host specific in req, upgrades the request, starts
// listeners for each port specified in ports, and forwards local connections
// to the remote host via streams.
// forward会dial在请求中指定的远程主机，升级请求，启动listener监听在ports中指定的各个端口
// 并且通过streams将本地的连接转发到远程主机
func (pf *PortForwarder) forward() error {
	var err error

	listenSuccess := false
	for _, port := range pf.ports {
		err = pf.listenOnPort(&port)
		switch {
		case err == nil:
			listenSuccess = true
		default:
			if pf.errOut != nil {
				fmt.Fprintf(pf.errOut, "Unable to listen on port %d: %v\n", port.Local, err)
			}
		}
	}

	if !listenSuccess {
		return fmt.Errorf("Unable to listen on any of the requested ports: %v", pf.ports)
	}

	if pf.Ready != nil {
		close(pf.Ready)
	}

	// wait for interrupt or conn closure
	select {
	case <-pf.stopChan:
	case <-pf.streamConn.CloseChan():
		runtime.HandleError(errors.New("lost connection to pod"))
	}

	return nil
}

// listenOnPort delegates tcp4 and tcp6 listener creation and waits for connections on both of these addresses.
// If both listener creation fail, an error is raised.
// listenOnPort会等待到达这些地址的连接
func (pf *PortForwarder) listenOnPort(port *ForwardedPort) error {
	errTcp4 := pf.listenOnPortAndAddress(port, "tcp4", "127.0.0.1")
	errTcp6 := pf.listenOnPortAndAddress(port, "tcp6", "[::1]")
	if errTcp4 != nil && errTcp6 != nil {
		return fmt.Errorf("All listeners failed to create with the following errors: %s, %s", errTcp4, errTcp6)
	}
	return nil
}

// listenOnPortAndAddress delegates listener creation and waits for new connections
// in the background f
// listenOnPortAndAddress创建listener并且在后台等待新的连接
func (pf *PortForwarder) listenOnPortAndAddress(port *ForwardedPort, protocol string, address string) error {
	listener, err := pf.getListener(protocol, address, port)
	if err != nil {
		return err
	}
	pf.listeners = append(pf.listeners, listener)
	go pf.waitForConnection(listener, *port)
	return nil
}

// getListener creates a listener on the interface targeted by the given hostname on the given port with
// the given protocol. protocol is in net.Listen style which basically admits values like tcp, tcp4, tcp6
// getListener创建一个listener，对于给定hostname的给定端口，在给定协议上
func (pf *PortForwarder) getListener(protocol string, hostname string, port *ForwardedPort) (net.Listener, error) {
	listener, err := net.Listen(protocol, net.JoinHostPort(hostname, strconv.Itoa(int(port.Local))))
	if err != nil {
		return nil, fmt.Errorf("Unable to create listener: Error %s", err)
	}
	listenerAddress := listener.Addr().String()
	host, localPort, _ := net.SplitHostPort(listenerAddress)
	localPortUInt, err := strconv.ParseUint(localPort, 10, 16)

	if err != nil {
		return nil, fmt.Errorf("Error parsing local port: %s from %s (%s)", err, listenerAddress, host)
	}
	port.Local = uint16(localPortUInt)
	if pf.out != nil {
		fmt.Fprintf(pf.out, "Forwarding from %s:%d -> %d\n", hostname, localPortUInt, port.Remote)
	}

	return listener, nil
}

// waitForConnection waits for new connections to listener and handles them in
// the background.
// waitForConnection等待到达listener的新的连接并且在后台对它们进行处理
func (pf *PortForwarder) waitForConnection(listener net.Listener, port ForwardedPort) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// TODO consider using something like https://github.com/hydrogen18/stoppableListener?
			if !strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
				runtime.HandleError(fmt.Errorf("Error accepting connection on port %d: %v", port.Local, err))
			}
			return
		}
		go pf.handleConnection(conn, port)
	}
}

func (pf *PortForwarder) nextRequestID() int {
	pf.requestIDLock.Lock()
	defer pf.requestIDLock.Unlock()
	id := pf.requestID
	pf.requestID++
	return id
}

// handleConnection copies data between the local connection and the stream to
// the remote server.
// handleConnection在本地连接和到远程主机的stream直接拷贝数据
func (pf *PortForwarder) handleConnection(conn net.Conn, port ForwardedPort) {
	defer conn.Close()

	if pf.out != nil {
		fmt.Fprintf(pf.out, "Handling connection for %d\n", port.Local)
	}

	// 创建request ID
	requestID := pf.nextRequestID()

	// create error stream
	// 创建error stream
	headers := http.Header{}
	headers.Set(v1.StreamType, v1.StreamTypeError)
	// 设置port forward stream的头部
	headers.Set(v1.PortHeader, fmt.Sprintf("%d", port.Remote))
	headers.Set(v1.PortForwardRequestIDHeader, strconv.Itoa(requestID))
	errorStream, err := pf.streamConn.CreateStream(headers)
	if err != nil {
		runtime.HandleError(fmt.Errorf("error creating error stream for port %d -> %d: %v", port.Local, port.Remote, err))
		return
	}
	// we're not writing to this stream
	errorStream.Close()

	errorChan := make(chan error)
	go func() {
		message, err := ioutil.ReadAll(errorStream)
		switch {
		case err != nil:
			errorChan <- fmt.Errorf("error reading from error stream for port %d -> %d: %v", port.Local, port.Remote, err)
		case len(message) > 0:
			errorChan <- fmt.Errorf("an error occurred forwarding %d -> %d: %v", port.Local, port.Remote, string(message))
		}
		close(errorChan)
	}()

	// create data stream
	// 创建data stream
	headers.Set(v1.StreamType, v1.StreamTypeData)
	dataStream, err := pf.streamConn.CreateStream(headers)
	if err != nil {
		runtime.HandleError(fmt.Errorf("error creating forwarding stream for port %d -> %d: %v", port.Local, port.Remote, err))
		return
	}

	localError := make(chan struct{})
	remoteDone := make(chan struct{})

	go func() {
		// Copy from the remote side to the local port.
		// 从远程主机复制数据到本地端口
		if _, err := io.Copy(conn, dataStream); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			runtime.HandleError(fmt.Errorf("error copying from remote stream to local connection: %v", err))
		}

		// inform the select below that the remote copy is done
		close(remoteDone)
	}()

	go func() {
		// inform server we're not sending any more data after copy unblocks
		defer dataStream.Close()

		// Copy from the local port to the remote side.
		// 从本地端口复制到远程主机
		if _, err := io.Copy(dataStream, conn); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			runtime.HandleError(fmt.Errorf("error copying from local connection to remote stream: %v", err))
			// break out of the select below without waiting for the other copy to finish
			close(localError)
		}
	}()

	// wait for either a local->remote error or for copying from remote->local to finish
	select {
	case <-remoteDone:
	case <-localError:
	}

	// always expect something on errorChan (it may be nil)
	err = <-errorChan
	if err != nil {
		runtime.HandleError(err)
	}
}

func (pf *PortForwarder) Close() {
	// stop all listeners
	for _, l := range pf.listeners {
		if err := l.Close(); err != nil {
			runtime.HandleError(fmt.Errorf("error closing listener: %v", err))
		}
	}
}
