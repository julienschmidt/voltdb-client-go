/* This file is part of VoltDB.
 * Copyright (C) 2008-2016 VoltDB Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with VoltDB.  If not, see <http://www.gnu.org/licenses/>.
 */

package voltdbclient

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var qHandle int64 = 0 // each query has a unique handle.

// connectionData are the values returned by a successful login.
type connectionData struct {
	hostId      int32
	connId      int64
	leaderAddr  int32
	buildString string
}

type connectionState struct {
	connInfo      string
	reader        io.Reader
	writer        io.Writer
	connData      connectionData
	asyncsChannel chan VoltResponse
	asyncs        map[int64]*VoltAsyncResponse
	asyncsMutex   sync.Mutex
	nl            *NetworkListener
	nlwg          *sync.WaitGroup
	isOpen        bool
}

type VoltConn struct {
	cs     *connectionState
}

func newVoltConn(connInfo string, reader io.Reader, writer io.Writer, connectionData connectionData) *VoltConn {
	var vc = new(VoltConn)

	asyncsChannel := make(chan VoltResponse)
	asyncs := make(map[int64]*VoltAsyncResponse)
	asyncsMutex := sync.Mutex{}
	wg := sync.WaitGroup{}
	nl := newListener(vc, reader, &wg)
	cs := connectionState{connInfo, reader, writer, connectionData, asyncsChannel, asyncs, asyncsMutex, nl, &wg, true}
	vc.cs = &cs
	nl.start()
	go vc.processAsyncs()
	return vc
}

func (vc VoltConn) Begin() (driver.Tx, error) {
	return nil, errors.New("VoltDB does not support transactions, VoltDB autocommits")
}

func (vc VoltConn) Close() (err error) {
	if !vc.isOpen() {
		return
	}

	// stop the network listener
	vc.nl().stop()

	// close the tcp conn, will unblock the listener.
	if vc.reader() != nil {
		tcpConn := vc.reader().(*net.TCPConn)
		err = tcpConn.Close()
	}

	// network thread should return.
	vc.nlwg().Wait()

	vc.cs.isOpen = false
	return err
}

func (vc VoltConn) Reconnect() {
	var first bool = true
	for {
		if first {
			first = false
		} else {
			time.Sleep(10 * time.Microsecond)
		}
		raddr, err := net.ResolveTCPAddr("tcp", vc.cs.connInfo)
		if err != nil {
			fmt.Printf("Failed to resolve tcp address of server %s\n", err)
			continue
		}
		tcpConn, err := net.DialTCP("tcp", nil, raddr)
		if err != nil {
			fmt.Printf("Failed to connect to server %s\n", err)
			continue
		}
		login, err := serializeLoginMessage("", "")
		if err != nil {
			fmt.Printf("Failed to serialize login message %s\n", err)
			continue
		}
		writeLoginMessage(tcpConn, &login)
		if err != nil {
			fmt.Printf("Failed to writing login message to server %s\n", err)
			continue
		}
		connectionData, err := readLoginResponse(tcpConn)
		if err != nil {
			fmt.Printf("Did not receive response to login request to server%s\n", err)
			continue
		}

		asyncs := make(map[int64]*VoltAsyncResponse)
		wg := sync.WaitGroup{}
		nl := newListener(&vc, tcpConn, &wg)

		vc.cs.reader = tcpConn
		vc.cs.writer = tcpConn
		vc.cs.connData = *connectionData
		vc.cs.asyncsChannel = make(chan VoltResponse)
		vc.cs.asyncs = asyncs
		vc.cs.asyncsMutex = sync.Mutex{}
		vc.cs.nl = nl
		vc.cs.nlwg = &wg
		vc.cs.isOpen = true
		nl.start()
		break
	}
}

func OpenConn(connInfo string) (*VoltConn, error) {
	// for now, at least, connInfo is host and port.
	raddr, err := net.ResolveTCPAddr("tcp", connInfo)
	if err != nil {
		return nil, fmt.Errorf("Error resolving %v.", connInfo)
	}
	var tcpConn *net.TCPConn
	if tcpConn, err = net.DialTCP("tcp", nil, raddr); err != nil {
		return nil, err
	}
	login, err := serializeLoginMessage("", "")
	if err != nil {
		return nil, err
	}
	writeLoginMessage(tcpConn, &login)
	connData, err := readLoginResponse(tcpConn)
	if err != nil {
		return nil, err
	}
	return newVoltConn(connInfo, tcpConn, tcpConn, *connData), nil
}

func (vc VoltConn) Prepare(query string) (driver.Stmt, error) {
	stmt := newVoltStatement(&vc, query)
	return *stmt, nil
}

func (vc VoltConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if !vc.isOpen() {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.nl().registerRequest(handle, false)
	if err := vc.serializeQuery(vc.writer(), query, handle, args); err != nil {
		vc.nl().removeRequest(handle)
		return VoltResult{}, err
	}
	resp := <-c
	rslt := resp.(VoltResult)
	if err := rslt.getError(); err != nil {
		return nil, err
	}
	return rslt, nil
}

func (vc VoltConn) ExecAsync(resCons resultConsumer, query string, args []driver.Value) (*VoltAsyncResponse, error) {
	if !vc.isOpen() {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.nl().registerRequest(handle, false)
	vasr := newVoltAsyncResponseForExec(vc, handle, c, resCons)
	vc.registerAsync(handle, vasr)
	if err := vc.serializeQuery(vc.writer(), query, handle, args); err != nil {
		vc.nl().removeRequest(handle)
		return nil, err
	}
	return vasr, nil
}

func (vc VoltConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if !vc.isOpen() {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.nl().registerRequest(handle, true)
	if err := vc.serializeQuery(vc.writer(), query, handle, args); err != nil {
		vc.nl().removeRequest(handle)
		return VoltRows{}, err
	}
	resp := <-c
	rows := resp.(VoltRows)
	if err := rows.getError(); err != nil {
		return nil, err
	}
	return rows, nil
}

func (vc VoltConn) QueryAsync(rowsCons rowsConsumer, query string, args []driver.Value) (*VoltAsyncResponse, error) {
	if !vc.isOpen() {
		return nil, errors.New("Connection is closed")
	}
	handle := atomic.AddInt64(&qHandle, 1)
	c := vc.nl().registerRequest(handle, true)
	vasr := newVoltAsyncResponseForQuery(vc, handle, c, rowsCons)
	vc.registerAsync(handle, vasr)
	if err := vc.serializeQuery(vc.writer(), query, handle, args); err != nil {
		vc.nl().removeRequest(handle)
		return nil, err
	}
	return vasr, nil
}

func (vc VoltConn) Drain() {
	var numAsyncs int
	for {
		vc.asyncsMutex().Lock()
		numAsyncs = len(vc.asyncs())
		vc.asyncsMutex().Unlock()
		if numAsyncs == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (vc VoltConn) processAsyncs() {
	for {
		resp := <- vc.cs.asyncsChannel
		handle := resp.Handle()
		vc.asyncsMutex().Lock()
		async := vc.asyncs()[handle]
		vc.asyncsMutex().Unlock()

		if async.isQuery() {
			vrows := resp.(VoltRows)
			if err := vrows.getError(); err != nil {
				async.rowsCons(nil, err)
			} else {
				async.rowsCons(vrows, nil)

			}
			vc.removeAsync(handle)
			continue
		} else {
			vrslt := resp.(VoltResult)
			if err := vrslt.getError(); err != nil {
				async.resCons(nil, err)
			} else {
				async.resCons(vrslt, nil)
			}
			vc.removeAsync(handle)
			continue
		}
	}
}

func (vc VoltConn) asyncs() map[int64]*VoltAsyncResponse {
	return vc.cs.asyncs
}

func (vc VoltConn) asyncsMutex() *sync.Mutex {
	return &vc.cs.asyncsMutex
}

func (vc VoltConn) isOpen() bool {
	return vc.cs.isOpen
}

func (vc VoltConn) nl() *NetworkListener {
	return vc.cs.nl
}

func (vc VoltConn) nlwg() *sync.WaitGroup {
	return vc.cs.nlwg
}

func (vc VoltConn) reader() io.Reader {
	return vc.cs.reader
}

func (vc VoltConn) writer() io.Writer {
	return vc.cs.writer
}

func (vc VoltConn) registerAsync(handle int64, vasr *VoltAsyncResponse) {
	vc.asyncsMutex().Lock()
	vc.asyncs()[handle] = vasr
	vc.asyncsMutex().Unlock()
	go func() {
		vc.cs.asyncsChannel <- <-vasr.channel()
	}()
}

func (vc VoltConn) removeAsync(han int64) {
	vc.asyncsMutex().Lock()
	delete(vc.asyncs(), han)
	vc.asyncsMutex().Unlock()
}

func writeLoginMessage(writer io.Writer, buf *bytes.Buffer) {
	// length includes protocol version.
	length := buf.Len() + 2
	var netmsg bytes.Buffer
	writeInt(&netmsg, int32(length))
	writeProtoVersion(&netmsg)
	writePasswordHashVersion(&netmsg)
	// 1 copy + 1 n/w write benchmarks faster than 2 n/w writes.
	io.Copy(&netmsg, buf)
	io.Copy(writer, &netmsg)
}

func readLoginResponse(reader io.Reader) (*connectionData, error) {
	buf, err := readMessage(reader)
	if err != nil {
		return nil, err
	}
	connData, err := deserializeLoginResponse(buf)
	return connData, err
}

type VoltAsyncResponse struct {
	conn     VoltConn
	han      int64
	ch       <-chan VoltResponse
	isQ      bool
	result   driver.Result
	rows     driver.Rows
	resCons  resultConsumer
	rowsCons rowsConsumer
}

type rowsConsumer func(driver.Rows, error)

type resultConsumer func(driver.Result, error)

func newVoltAsyncResponseForExec(conn VoltConn, han int64, ch <-chan VoltResponse, resCons resultConsumer) *VoltAsyncResponse {
	var vasr = new(VoltAsyncResponse)
	vasr.conn = conn
	vasr.han = han
	vasr.ch = ch
	vasr.isQ = false
	vasr.resCons = resCons
	vasr.rowsCons = nil
	return vasr
}

func newVoltAsyncResponseForQuery(conn VoltConn, han int64, ch <-chan VoltResponse, rowsCons rowsConsumer) *VoltAsyncResponse {
	var vasr = new(VoltAsyncResponse)
	vasr.conn = conn
	vasr.han = han
	vasr.ch = ch
	vasr.isQ = true
	vasr.resCons = nil
	vasr.rowsCons = rowsCons
	return vasr
}

func (vasr *VoltAsyncResponse) channel() <-chan VoltResponse {
	return vasr.ch
}

func (vasr *VoltAsyncResponse) handle() int64 {
	return vasr.han
}

func (vasr *VoltAsyncResponse) isQuery() bool {
	return vasr.isQ
}

func (vc VoltConn) serializeQuery(writer io.Writer, procedure string, handle int64, args []driver.Value) error {

	var call bytes.Buffer
	var err error

	// Serialize the procedure call and its params.
	// Use 0 for handle; it's not necessary in pure sync client.
	if call, err = serializeStatement(procedure, handle, args); err != nil {
		return err
	}

	var netmsg bytes.Buffer
	writeInt(&netmsg, int32(call.Len()))
	io.Copy(&netmsg, &call)
	io.Copy(writer, &netmsg)
	return nil
}

// Null Value type
type NullValue struct {
	colType int8
}

func NewNullValue(colType int8) *NullValue {
	var nv = new(NullValue)
	nv.colType = colType
	return nv
}

func (nv *NullValue) ColType() int8 {
	return nv.colType
}
