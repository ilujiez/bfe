// Copyright (c) 2019 The BFE Authors.
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

// Copyright (c) pires.
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

package bfe_proxy

import (
	"bytes"
	"encoding/binary"
	"io"
)

import (
	bufio "github.com/bfenetworks/bfe/bfe_bufio"
)

var (
	lengthV4   = uint16(12)
	lengthV6   = uint16(36)
	lengthUnix = uint16(218)

	lengthV4Bytes = func() []byte {
		a := make([]byte, 2)
		binary.BigEndian.PutUint16(a, lengthV4)
		return a
	}()
	lengthV6Bytes = func() []byte {
		a := make([]byte, 2)
		binary.BigEndian.PutUint16(a, lengthV6)
		return a
	}()
	lengthUnixBytes = func() []byte {
		a := make([]byte, 2)
		binary.BigEndian.PutUint16(a, lengthUnix)
		return a
	}()
)

type _ports struct {
	SrcPort uint16
	DstPort uint16
}

type _addr4 struct {
	Src     [4]byte
	Dst     [4]byte
	SrcPort uint16
	DstPort uint16
}

type _addr6 struct {
	Src [16]byte
	Dst [16]byte
	_ports
}

// parseVersion2 parses protocol header
//
// Note: binary header format:
// - Signature [1~12]
// - Version and Command [13]
// - Protocol and Address Family [14]
// - Address Length [15]
// - Additional TLVs [optional]
func parseVersion2(reader *bufio.Reader) (header *Header, err error) {
	// Skip first 12 bytes (signature)
	for i := 0; i < 12; i++ {
		if _, err = reader.ReadByte(); err != nil {
			state.ProxyErrReadHeader.Inc(1)
			return nil, ErrCantReadProtocolVersionAndCommand
		}
	}

	header = new(Header)
	header.Version = 2

	// Read the 13th byte, protocol version and command
	b13, err := reader.ReadByte()
	if err != nil {
		state.ProxyErrReadHeader.Inc(1)
		return nil, ErrCantReadProtocolVersionAndCommand
	}
	header.Command = ProtocolVersionAndCommand(b13)
	if _, ok := supportedCommand[header.Command]; !ok {
		state.ProxyErrInvalidHeader.Inc(1)
		return nil, ErrUnsupportedProtocolVersionAndCommand
	}
	// If command is LOCAL, header ends here
	if header.Command.IsLocal() {
		return header, nil
	}

	// Read the 14th byte, address family and protocol
	b14, err := reader.ReadByte()
	if err != nil {
		state.ProxyErrReadHeader.Inc(1)
		return nil, ErrCantReadAddressFamilyAndProtocol
	}
	header.TransportProtocol = AddressFamilyAndProtocol(b14)
	if _, ok := supportedTransportProtocol[header.TransportProtocol]; !ok {
		state.ProxyErrInvalidHeader.Inc(1)
		return nil, ErrUnsupportedAddressFamilyAndProtocol
	}

	// Make sure there are bytes available as specified in length
	var length uint16
	if err := binary.Read(io.LimitReader(reader, 2), binary.BigEndian, &length); err != nil {
		state.ProxyErrReadHeader.Inc(1)
		return nil, ErrCantReadLength
	}

	if !header.validateLength(length) {
		state.ProxyErrInvalidHeader.Inc(1)
		return nil, ErrInvalidLength
	}

	if _, err := reader.Peek(int(length)); err != nil {
		state.ProxyErrReadHeader.Inc(1)
		return nil, ErrInvalidLength
	}

	// Length-limited reader for payload section
	payloadReader := io.LimitReader(reader, int64(length))

	// Read addresses and ports
	if header.TransportProtocol.IsIPv4() {
		var addr _addr4
		if err := binary.Read(payloadReader, binary.BigEndian, &addr); err != nil {
			state.ProxyErrReadHeader.Inc(1)
			return nil, ErrInvalidAddress
		}
		header.SourceAddress = addr.Src[:]
		header.DestinationAddress = addr.Dst[:]
		header.SourcePort = addr.SrcPort
		header.DestinationPort = addr.DstPort
	} else if header.TransportProtocol.IsIPv6() {
		var addr _addr6
		if err := binary.Read(payloadReader, binary.BigEndian, &addr); err != nil {
			state.ProxyErrReadHeader.Inc(1)
			return nil, ErrInvalidAddress
		}
		header.SourceAddress = addr.Src[:]
		header.DestinationAddress = addr.Dst[:]
		header.SourcePort = addr.SrcPort
		header.DestinationPort = addr.DstPort
	}

	// TODO add encapsulated TLV support

	// Drain the remaining padding
	payloadReader.Read(make([]byte, length))

	state.ProxyNormalV2Header.Inc(1)
	return header, nil
}

func (header *Header) writeVersion2(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	buf.Write(SIGV2)
	buf.WriteByte(header.Command.toByte())
	if !header.Command.IsLocal() {
		buf.WriteByte(header.TransportProtocol.toByte())
		// TODO add encapsulated TLV length
		var addrSrc, addrDst []byte
		if header.TransportProtocol.IsIPv4() {
			buf.Write(lengthV4Bytes)
			addrSrc = header.SourceAddress.To4()
			addrDst = header.DestinationAddress.To4()
		} else if header.TransportProtocol.IsIPv6() {
			buf.Write(lengthV6Bytes)
			addrSrc = header.SourceAddress.To16()
			addrDst = header.DestinationAddress.To16()
		} else if header.TransportProtocol.IsUnix() {
			buf.Write(lengthUnixBytes)
			// TODO is below right?
			addrSrc = []byte(header.SourceAddress.String())
			addrDst = []byte(header.DestinationAddress.String())
		}
		buf.Write(addrSrc)
		buf.Write(addrDst)

		portSrcBytes := func() []byte {
			a := make([]byte, 2)
			binary.BigEndian.PutUint16(a, header.SourcePort)
			return a
		}()
		buf.Write(portSrcBytes)

		portDstBytes := func() []byte {
			a := make([]byte, 2)
			binary.BigEndian.PutUint16(a, header.DestinationPort)
			return a
		}()
		buf.Write(portDstBytes)

	}

	return buf.WriteTo(w)
}

func (header *Header) validateLength(length uint16) bool {
	if header.TransportProtocol.IsIPv4() {
		return length >= lengthV4
	} else if header.TransportProtocol.IsIPv6() {
		return length >= lengthV6
	} else if header.TransportProtocol.IsUnix() {
		return length >= lengthUnix
	}
	return false
}
