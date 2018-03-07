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

package remotecommand

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// streamProtocolV1 implements the first version of the streaming exec & attach
// protocol. This version has some bugs, such as not being able to detect when
// non-interactive stdin data has ended. See http://issues.k8s.io/13394 and
// http://issues.k8s.io/13395 for more details.
type streamProtocolV1 struct {
	StreamOptions

	errorStream  httpstream.Stream
	remoteStdin  httpstream.Stream
	remoteStdout httpstream.Stream
	remoteStderr httpstream.Stream
}

var _ streamProtocolHandler = &streamProtocolV1{}

func newStreamProtocolV1(options StreamOptions) streamProtocolHandler {
	return &streamProtocolV1{
		StreamOptions: options,
	}
}

// streamCreator是一个interface，要求能实现CreateStream这个方法
func (p *streamProtocolV1) stream(conn streamCreator) error {
	doneChan := make(chan struct{}, 2)
	errorChan := make(chan error)

	cp := func(s string, dst io.Writer, src io.Reader) {
		glog.V(6).Infof("Copying %s", s)
		defer glog.V(6).Infof("Done copying %s", s)
		// 当读出的错误为io.EOF时，则结束
		if _, err := io.Copy(dst, src); err != nil && err != io.EOF {
			glog.Errorf("Error copying %s: %v", s, err)
		}
		if s == v1.StreamTypeStdout || s == v1.StreamTypeStderr {
			// 如果为stdout，或者stderr就发送给doneChan
			doneChan <- struct{}{}
		}
	}

	// set up all the streams first
	var err error
	headers := http.Header{}
	// 首先创建一个error stream
	headers.Set(v1.StreamType, v1.StreamTypeError)
	p.errorStream, err = conn.CreateStream(headers)
	if err != nil {
		return err
	}
	defer p.errorStream.Reset()

	// Create all the streams first, then start the copy goroutines. The server doesn't start its copy
	// goroutines until it's received all of the streams. If the client creates the stdin stream and
	// immediately begins copying stdin data to the server, it's possible to overwhelm and wedge the
	// spdy frame handler in the server so that it is full of unprocessed frames. The frames aren't
	// getting processed because the server hasn't started its copying, and it won't do that until it
	// gets all the streams. By creating all the streams first, we ensure that the server is ready to
	// process data before the client starts sending any. See https://issues.k8s.io/16373 for more info.
	// 我们必须创建好所有的stream，不然server端不会进行拷贝工作
	if p.Stdin != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdin)
		p.remoteStdin, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStdin.Reset()
	}

	if p.Stdout != nil {
		headers.Set(v1.StreamType, v1.StreamTypeStdout)
		p.remoteStdout, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStdout.Reset()
	}

	if p.Stderr != nil && !p.Tty {
		headers.Set(v1.StreamType, v1.StreamTypeStderr)
		p.remoteStderr, err = conn.CreateStream(headers)
		if err != nil {
			return err
		}
		defer p.remoteStderr.Reset()
	}

	// now that all the streams have been created, proceed with reading & copying
	// 现在所有stream都准备就绪了，可以进行reading & copying了

	// always read from errorStream
	// 如果从errorStream读到错误信息就报错
	go func() {
		message, err := ioutil.ReadAll(p.errorStream)
		if err != nil && err != io.EOF {
			errorChan <- fmt.Errorf("Error reading from error stream: %s", err)
			return
		}
		if len(message) > 0 {
			errorChan <- fmt.Errorf("Error executing remote command: %s", message)
			return
		}
	}()

	if p.Stdin != nil {
		// TODO this goroutine will never exit cleanly (the io.Copy never unblocks)
		// because stdin is not closed until the process exits. If we try to call
		// stdin.Close(), it returns no error but doesn't unblock the copy. It will
		// exit when the process exits, instead.
		go cp(v1.StreamTypeStdin, p.remoteStdin, p.Stdin)
	}

	waitCount := 0
	completedStreams := 0

	if p.Stdout != nil {
		waitCount++
		go cp(v1.StreamTypeStdout, p.Stdout, p.remoteStdout)
	}

	if p.Stderr != nil && !p.Tty {
		waitCount++
		go cp(v1.StreamTypeStderr, p.Stderr, p.remoteStderr)
	}

Loop:
	for {
		select {
		case <-doneChan:
			completedStreams++
			// 等待stream完成
			if completedStreams == waitCount {
				break Loop
			}
		case err := <-errorChan:
			return err
		}
	}

	return nil
}
