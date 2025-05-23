/*
Copyright 2019 The Vitess Authors.

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

package mysql

import (
	"fmt"
	"math"

	"vitess.io/vitess/go/mysql/sqlerror"
	"vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
)

const (
	semiSyncIndicator    byte = 0xef
	semiSyncAckRequested byte = 0x01
)

// This file contains the methods related to replication.

// WriteComBinlogDump writes a ComBinlogDump command.
// Returns a SQLError.
// See: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_binlog_dump.html
func (c *Conn) WriteComBinlogDump(serverID uint32, binlogFilename string, binlogPos uint64, flags uint16) error {
	// The binary log file position is a uint64, but the protocol command
	// only uses 4 bytes for the file position.
	if binlogPos > math.MaxUint32 {
		return vterrors.Errorf(vtrpc.Code_INVALID_ARGUMENT, "binlog position %d is too large, it must fit into 32 bits", binlogPos)
	}
	c.sequence = 0
	length := 1 + // ComBinlogDump
		4 + // binlog-pos
		2 + // flags
		4 + // server-id
		len(binlogFilename) // binlog-filename
	data, pos := c.startEphemeralPacketWithHeader(length)
	pos = writeByte(data, pos, ComBinlogDump)
	pos = writeUint32(data, pos, uint32(binlogPos))
	pos = writeUint16(data, pos, flags)
	pos = writeUint32(data, pos, serverID)
	_ = writeEOFString(data, pos, binlogFilename)
	if err := c.writeEphemeralPacket(); err != nil {
		return sqlerror.NewSQLErrorf(sqlerror.CRServerGone, sqlerror.SSUnknownSQLState, "%v", err)
	}
	return nil
}

// AnalyzeSemiSyncAckRequest is given a packet and checks if the packet has a semi-sync Ack request.
// This is only applicable to binlog dump event packets.
// If semi sync information exists, then the function returns a stopped buf that should be then
// processed as the event data
func (c *Conn) AnalyzeSemiSyncAckRequest(buf []byte) (strippedBuf []byte, ackRequested bool, err error) {
	if !c.ExpectSemiSyncIndicator {
		return buf, false, nil
	}
	// semi sync indicator is expected
	// see https://dev.mysql.com/doc/internals/en/semi-sync-binlog-event.html
	if len(buf) < 2 {
		return buf, false, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "semi sync indicator expected, but packet too small")
	}
	if buf[0] != semiSyncIndicator {
		return buf, false, vterrors.Errorf(vtrpc.Code_FAILED_PRECONDITION, "semi sync indicator expected, but not found")
	}
	return buf[2:], buf[1] == semiSyncAckRequested, nil
}

// WriteComBinlogDumpGTID writes a ComBinlogDumpGTID command.
// Only works with MySQL 5.6+ (and not MariaDB).
// See http://dev.mysql.com/doc/internals/en/com-binlog-dump-gtid.html for syntax.
// sidBlock must be the result of a gtidSet.SIDBlock() function.
func (c *Conn) WriteComBinlogDumpGTID(serverID uint32, binlogFilename string, binlogPos uint64, flags uint16, sidBlock []byte) error {
	c.sequence = 0
	length := 1 + // ComBinlogDumpGTID
		2 + // flags
		4 + // server-id
		4 + // binlog-filename-len
		len(binlogFilename) + // binlog-filename
		8 + // binlog-pos
		4 + // data-size
		len(sidBlock) // data
	data, pos := c.startEphemeralPacketWithHeader(length)
	pos = writeByte(data, pos, ComBinlogDumpGTID)             // nolint
	pos = writeUint16(data, pos, flags)                       // nolint
	pos = writeUint32(data, pos, serverID)                    // nolint
	pos = writeUint32(data, pos, uint32(len(binlogFilename))) // nolint
	pos = writeEOFString(data, pos, binlogFilename)           // nolint
	pos = writeUint64(data, pos, binlogPos)                   // nolint
	pos = writeUint32(data, pos, uint32(len(sidBlock)))       // nolint
	pos += copy(data[pos:], sidBlock)                         // nolint
	if err := c.writeEphemeralPacket(); err != nil {
		return sqlerror.NewSQLErrorf(sqlerror.CRServerGone, sqlerror.SSUnknownSQLState, "%v", err)
	}
	return nil
}

// SendSemiSyncAck sends an ACK to the source, in response to binlog events
// the source has tagged with a SEMI_SYNC_ACK_REQ
// see https://dev.mysql.com/doc/internals/en/semi-sync-ack-packet.html
func (c *Conn) SendSemiSyncAck(binlogFilename string, binlogPos uint64) error {
	c.sequence = 0
	length := 1 + // ComSemiSyncAck
		8 + // binlog-pos
		len(binlogFilename) // binlog-filename
	data, pos := c.startEphemeralPacketWithHeader(length)
	pos = writeByte(data, pos, ComSemiSyncAck)
	pos = writeUint64(data, pos, binlogPos)
	_ = writeEOFString(data, pos, binlogFilename)
	if err := c.writeEphemeralPacket(); err != nil {
		return sqlerror.NewSQLErrorf(sqlerror.CRServerGone, sqlerror.SSUnknownSQLState, "%v", err)
	}
	return nil

}

// WriteBinlogEvent writes a binlog event as part of a replication stream
// https://dev.mysql.com/doc/internals/en/binlog-network-stream.html
// https://dev.mysql.com/doc/internals/en/binlog-event.html
func (c *Conn) WriteBinlogEvent(ev BinlogEvent, semiSyncEnabled bool) error {
	extraBytes := 1 // OK packet
	if semiSyncEnabled {
		extraBytes += 2
	}
	data, pos := c.startEphemeralPacketWithHeader(len(ev.Bytes()) + extraBytes)
	pos = writeByte(data, pos, 0) // "OK" prefix
	if semiSyncEnabled {
		pos = writeByte(data, pos, 0xef) // semi sync indicator
		pos = writeByte(data, pos, 0)    // no ack expected
	}
	_ = writeEOFString(data, pos, string(ev.Bytes()))
	if err := c.writeEphemeralPacket(); err != nil {
		return sqlerror.NewSQLErrorf(sqlerror.CRServerGone, sqlerror.SSUnknownSQLState, "%v", err)
	}
	return nil
}

type SemiSyncType int8

const (
	SemiSyncTypeUnknown SemiSyncType = iota
	SemiSyncTypeOff
	SemiSyncTypeSource
	SemiSyncTypeMaster
)

// SemiSyncExtensionLoaded checks if the semisync extension has been loaded.
// It should work for both MariaDB and MySQL.
func (c *Conn) SemiSyncExtensionLoaded() (SemiSyncType, error) {
	qr, err := c.ExecuteFetch("SHOW VARIABLES LIKE 'rpl_semi_sync_%_enabled'", 10, false)
	if err != nil {
		return SemiSyncTypeUnknown, err
	}
	for _, row := range qr.Rows {
		if row[0].ToString() == "rpl_semi_sync_source_enabled" {
			return SemiSyncTypeSource, nil
		}
		if row[0].ToString() == "rpl_semi_sync_master_enabled" {
			return SemiSyncTypeMaster, nil
		}
	}
	return SemiSyncTypeOff, nil
}

func (c *Conn) BinlogInformation() (string, bool, bool, string, error) {
	replicaField := c.flavor.binlogReplicatedUpdates()

	query := fmt.Sprintf("select @@global.binlog_format, @@global.log_bin, %s, @@global.binlog_row_image", replicaField)
	qr, err := c.ExecuteFetch(query, 1, true)
	if err != nil {
		return "", false, false, "", err
	}
	if len(qr.Rows) != 1 {
		return "", false, false, "", fmt.Errorf("unable to read global variables binlog_format, log_bin, %s, binlog_row_image", replicaField)
	}
	res := qr.Named().Row()
	binlogFormat, err := res.ToString("@@global.binlog_format")
	if err != nil {
		return "", false, false, "", err
	}
	logBin, err := res.ToInt64("@@global.log_bin")
	if err != nil {
		return "", false, false, "", err
	}
	logReplicaUpdates, err := res.ToInt64(replicaField)
	if err != nil {
		return "", false, false, "", err
	}
	binlogRowImage, err := res.ToString("@@global.binlog_row_image")
	if err != nil {
		return "", false, false, "", err
	}
	return binlogFormat, logBin == 1, logReplicaUpdates == 1, binlogRowImage, nil
}

// ResetBinaryLogsCommand returns the command used to reset the
// binary logs on the server.
func (c *Conn) ResetBinaryLogsCommand() string {
	return c.flavor.resetBinaryLogsCommand()
}
