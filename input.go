//
// Fluentd Forwarder
//
// Copyright (C) 2014 Treasure Data, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package fluentd_forwarder

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	logging "github.com/op/go-logging"
	"github.com/ugorji/go/codec"
	"io"
	"net"
	"reflect"
	"regexp"
	"sync"
	"sync/atomic"
)

var (
	listenAddrRegexp = regexp.MustCompile("^(tcp|unix)://(.+)$")
)

type ForwardListener interface {
	io.Closer
	Accept() (c net.Conn, err error)
}

type ForwardConn interface {
	io.Reader
	io.Closer
	RemoteAddr() net.Addr
}

type forwardClient struct {
	input        *ForwardInput
	logger       *logging.Logger
	conn         ForwardConn
	msgpackCodec *codec.MsgpackHandle
	jsonCodec    *codec.JsonHandle
	reader       *bufio.Reader
}

type ForwardInput struct {
	entries        int64 // This variable must be on 64-bit alignment. Otherwise atomic.AddInt64 will cause a crash on ARM and x86-32
	port           Port
	logger         *logging.Logger
	bind           string
	listener       ForwardListener
	msgpackCodec   *codec.MsgpackHandle
	jsonCodec      *codec.JsonHandle
	clientsMtx     sync.Mutex
	clients        map[ForwardConn]*forwardClient
	wg             sync.WaitGroup
	acceptChan     chan ForwardConn
	shutdownChan   chan struct{}
	isShuttingDown uintptr
}

type EntryCountTopic struct{}

type ConnectionCountTopic struct{}

type ForwardInputFactory struct{}

func coerceInPlace(data map[string]interface{}) {
	for k, v := range data {
		switch v_ := v.(type) {
		case []byte:
			data[k] = string(v_) // XXX: byte => rune
		case map[string]interface{}:
			coerceInPlace(v_)
		}
	}
}

func (c *forwardClient) decodeRecordSet(tag string, entries []interface{}) (FluentRecordSet, error) {
	records := make([]TinyFluentRecord, len(entries))
	for i, _entry := range entries {
		entry, ok := _entry.([]interface{})
		if !ok {
			return FluentRecordSet{}, errors.New("Failed to decode recordSet")
		}
		timestamp, ok := entry[0].(uint64)
		if !ok {
			return FluentRecordSet{}, errors.New("Failed to decode timestamp field")
		}
		data, ok := entry[1].(map[string]interface{})
		if !ok {
			return FluentRecordSet{}, errors.New("Failed to decode data field")
		}
		coerceInPlace(data)
		records[i] = TinyFluentRecord{
			Timestamp: timestamp,
			Data:      data,
		}
	}
	return FluentRecordSet{
		Tag:     tag,
		Records: records,
	}, nil
}

func (c *forwardClient) decodeEntries() ([]FluentRecordSet, error) {
	start, err := c.reader.Peek(1)
	if err != nil {
		return nil, err
	}

	var _codec codec.Handle
	switch start[0] {
	case '{', '[':
		_codec = c.jsonCodec
	default:
		_codec = c.msgpackCodec
	}
	dec := codec.NewDecoder(c.reader, _codec)

	v := []interface{}{nil, nil, nil}
	err = dec.Decode(&v)
	if err != nil {
		return nil, err
	}

	var tag string
	switch _tag := v[0].(type) {
	case []byte:
		tag = string(_tag) // XXX: byte => rune
	case string:
		tag = _tag
	default:
		return nil, errors.New("Failed to decode tag field")
	}

	var retval []FluentRecordSet
	switch timestamp_or_entries := v[1].(type) {
	case uint64:
		timestamp := timestamp_or_entries
		data, ok := v[2].(map[string]interface{})
		if !ok {
			return nil, errors.New("Failed to decode data field")
		}
		coerceInPlace(data)
		retval = []FluentRecordSet{
			{
				Tag: tag,
				Records: []TinyFluentRecord{
					{
						Timestamp: timestamp,
						Data:      data,
					},
				},
			},
		}
	case float64:
		timestamp := uint64(timestamp_or_entries)
		data, ok := v[2].(map[string]interface{})
		if !ok {
			return nil, errors.New("Failed to decode data field")
		}
		retval = []FluentRecordSet{
			{
				Tag: tag,
				Records: []TinyFluentRecord{
					{
						Timestamp: timestamp,
						Data:      data,
					},
				},
			},
		}
	case []interface{}:
		recordSet, err := c.decodeRecordSet(tag, timestamp_or_entries)
		if err != nil {
			return nil, err
		}
		retval = []FluentRecordSet{recordSet}
	case []byte:
		entries := make([]interface{}, 0)
		reader := bytes.NewReader(timestamp_or_entries)
		dec := codec.NewDecoder(reader, _codec)
		for reader.Len() > 0 { // codec.Decoder doesn't return EOF.
			entry := []interface{}{}
			err := dec.Decode(&entry)
			if err != nil {
				if err == io.EOF { // in case codec.Decoder changes its behavior
					break
				}
				return nil, err
			}
			entries = append(entries, entry)
		}
		recordSet, err := c.decodeRecordSet(tag, entries)
		if err != nil {
			return nil, err
		}
		retval = []FluentRecordSet{recordSet}
	default:
		return nil, errors.New(fmt.Sprintf("Unknown type: %t", timestamp_or_entries))
	}
	atomic.AddInt64(&c.input.entries, int64(len(retval)))
	return retval, nil
}

func (c *forwardClient) startHandling() {
	c.input.wg.Add(1)
	go func() {
		defer func() {
			err := c.conn.Close()
			if err != nil {
				c.logger.Debugf("Close: %s", err.Error())
			}
			c.input.markDischarged(c)
			c.input.wg.Done()
		}()
		remoteAddr := c.conn.RemoteAddr().String()
		c.input.logger.Infof("Started handling connection from %s", remoteAddr)
		for {
			recordSets, err := c.decodeEntries()
			if err != nil {
				err_, ok := err.(net.Error)
				if ok {
					if err_.Temporary() {
						c.logger.Infof("Temporary failure: %s", err_.Error())
						continue
					}
				}
				if err == io.EOF {
					c.logger.Infof("Client %s closed the connection", remoteAddr)
				} else {
					c.logger.Error(err.Error())
				}
				break
			}

			if len(recordSets) > 0 {
				err_ := c.input.port.Emit(recordSets)
				if err_ != nil {
					c.logger.Error(err_.Error())
					break
				}
			}
		}
		c.input.logger.Infof("Ended handling connection from %s", remoteAddr)
	}()
}

func (c *forwardClient) shutdown() {
	err := c.conn.Close()
	if err != nil {
		c.input.logger.Infof("Error during closing connection: %s", err.Error())
	}
}

func newForwardClient(input *ForwardInput, logger *logging.Logger, conn ForwardConn, msgpackCodec *codec.MsgpackHandle, jsonCodec *codec.JsonHandle) *forwardClient {
	c := &forwardClient{
		input:        input,
		logger:       logger,
		conn:         conn,
		msgpackCodec: msgpackCodec,
		jsonCodec:    jsonCodec,
		reader:       bufio.NewReader(conn),
	}
	input.markCharged(c)
	return c
}

func (input *ForwardInput) spawnAcceptor() {
	input.logger.Notice("Spawning acceptor")
	input.wg.Add(1)
	go func() {
		defer func() {
			close(input.acceptChan)
			input.wg.Done()
		}()
		input.logger.Notice("Acceptor started")
		for {
			conn, err := input.listener.Accept()
			if err != nil {
				input.logger.Notice(err.Error())
				break
			}
			if conn != nil {
				input.logger.Noticef("Connected from %s", conn.RemoteAddr().String())
				input.acceptChan <- conn
			} else {
				input.logger.Notice("Accept returned nil; something went wrong")
				break
			}
		}
		input.logger.Notice("Acceptor ended")
	}()
}

func (input *ForwardInput) spawnDaemon() {
	input.logger.Notice("Spawning daemon")
	input.wg.Add(1)
	go func() {
		defer func() {
			close(input.shutdownChan)
			input.wg.Done()
		}()
		input.logger.Notice("Daemon started")
	loop:
		for {
			select {
			case conn := <-input.acceptChan:
				if conn != nil {
					input.logger.Notice("Got conn from acceptChan")
					newForwardClient(input, input.logger, conn, input.msgpackCodec, input.jsonCodec).startHandling()
				}
			case <-input.shutdownChan:
				input.listener.Close()
				for _, client := range input.clients {
					client.shutdown()
				}
				break loop
			}
		}
		input.logger.Notice("Daemon ended")
	}()
}

func (input *ForwardInput) markCharged(c *forwardClient) {
	input.clientsMtx.Lock()
	defer input.clientsMtx.Unlock()
	input.clients[c.conn] = c
}

func (input *ForwardInput) markDischarged(c *forwardClient) {
	input.clientsMtx.Lock()
	defer input.clientsMtx.Unlock()
	delete(input.clients, c.conn)
}

func (input *ForwardInput) String() string {
	return "input"
}

func (input *ForwardInput) Start() {
	input.spawnAcceptor()
	input.spawnDaemon()
}

func (input *ForwardInput) WaitForShutdown() {
	input.wg.Wait()
}

func (input *ForwardInput) Stop() {
	if atomic.CompareAndSwapUintptr(&input.isShuttingDown, uintptr(0), uintptr(1)) {
		input.shutdownChan <- struct{}{}
	}
}

func NewForwardInput(logger *logging.Logger, bind string, port Port) (*ForwardInput, error) {
	mapType := reflect.TypeOf(map[string]interface{}(nil))
	sliceType := reflect.TypeOf([]interface{}{nil, nil, nil})

	msgpackCodec := codec.MsgpackHandle{}
	msgpackCodec.MapType = mapType
	msgpackCodec.RawToString = false

	jsonCodec := codec.JsonHandle{}
	jsonCodec.MapType = mapType
	jsonCodec.SliceType = sliceType

	network, address, err := parseNetworkAddress(bind)
	if err != nil {
		logger.Error(err.Error())
		return nil, err
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		logger.Error(err.Error())
		return nil, err
	}
	return &ForwardInput{
		entries:        0,
		port:           port,
		logger:         logger,
		bind:           bind,
		listener:       listener,
		msgpackCodec:   &msgpackCodec,
		jsonCodec:      &jsonCodec,
		clientsMtx:     sync.Mutex{},
		clients:        make(map[ForwardConn]*forwardClient),
		wg:             sync.WaitGroup{},
		acceptChan:     make(chan ForwardConn),
		shutdownChan:   make(chan struct{}),
		isShuttingDown: uintptr(0),
	}, nil
}

func parseNetworkAddress(address string) (string, string, error) {
	match := listenAddrRegexp.FindStringSubmatch(address)
	if len(match) != 3 {
		return "", "", errors.New("Failed to parse listen address")
	}

	return match[1], match[2], nil
}
