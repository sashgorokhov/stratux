package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
	"encoding/json"
)

// http://www.faa.gov/nextgen/programs/adsb/wsa/media/GDL90_Public_ICD_RevA.PDF

const (
	stratuxVersion			= "v0.1"
	configLocation			= "stratux.conf"
	ipadAddr                = "192.168.10.255:4000" // Port 4000 for FreeFlight RANGR.
	maxDatagramSize         = 8192
	UPLINK_BLOCK_DATA_BITS  = 576
	UPLINK_BLOCK_BITS       = (UPLINK_BLOCK_DATA_BITS + 160)
	UPLINK_BLOCK_DATA_BYTES = (UPLINK_BLOCK_DATA_BITS / 8)
	UPLINK_BLOCK_BYTES      = (UPLINK_BLOCK_BITS / 8)

	UPLINK_FRAME_BLOCKS     = 6
	UPLINK_FRAME_DATA_BITS  = (UPLINK_FRAME_BLOCKS * UPLINK_BLOCK_DATA_BITS)
	UPLINK_FRAME_BITS       = (UPLINK_FRAME_BLOCKS * UPLINK_BLOCK_BITS)
	UPLINK_FRAME_DATA_BYTES = (UPLINK_FRAME_DATA_BITS / 8)
	UPLINK_FRAME_BYTES      = (UPLINK_FRAME_BITS / 8)

	// assume 6 byte frames: 2 header bytes, 4 byte payload
	// (TIS-B heartbeat with one address, or empty FIS-B APDU)
	UPLINK_MAX_INFO_FRAMES = (424 / 6)

	MSGTYPE_UPLINK       = 0x07
	MSGTYPE_BASIC_REPORT = 0x1E
	MSGTYPE_LONG_REPORT  = 0x1F

	MSGCLASS_UAT		 = 0
	MSGCLASS_ES			 = 1
)

var Crc16Table [256]uint16
var outConn *net.UDPConn

type msg struct {
	MessageClass	uint
	TimeReceived	time.Time
	Data			[]byte
}

var MsgLog []msg

// Construct the CRC table. Adapted from FAA ref above.
func crcInit() {
	var i uint16
	var bitctr uint16
	var crc uint16
	for i = 0; i < 256; i++ {
		crc = (i << 8)
		for bitctr = 0; bitctr < 8; bitctr++ {
			z := uint16(0)
			if (crc & 0x8000) != 0 {
				z = 0x1021
			}
			crc = (crc << 1) ^ z
		}
		Crc16Table[i] = crc
	}
}

// Compute CRC. Adapted from FAA ref above.
func crcCompute(data []byte) uint16 {
	ret := uint16(0)
	for i := 0; i < len(data); i++ {
		ret = Crc16Table[ret>>8] ^ (ret << 8) ^ uint16(data[i])
	}
	return ret
}

func prepareMessage(data []byte) []byte {
	// Compute CRC before modifying the message.
	crc := crcCompute(data)
	// Add the two CRC16 bytes before replacing control characters.
	data = append(data, byte(crc&0xFF))
	data = append(data, byte(crc>>8))

	tmp := []byte{0x7E} // Flag start.

	// Copy the message over, escaping 0x7E (Flag Byte) and 0x7D (Control-Escape).
	for i := 0; i < len(data); i++ {
		mv := data[i]
		if (mv == 0x7E) || (mv == 0x7D) {
			mv = mv ^ 0x20
			tmp = append(tmp, 0x7D)
		}
		tmp = append(tmp, mv)
	}

	tmp = append(tmp, 0x7E) // Flag end.

	return tmp
}


func makeHeartbeat() []byte {
	msg := make([]byte, 7)
	// See p.10.
	msg[0] = 0x00 // Message type "Heartbeat".
	msg[1] = 0x01 // "UAT Initialized".
	nowUTC := time.Now().UTC()
	// Seconds since 0000Z.
	midnightUTC := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
	secondsSinceMidnightUTC := uint32(nowUTC.Sub(midnightUTC).Seconds())

	msg[2] = byte((secondsSinceMidnightUTC >> 16) << 7)
	msg[3] = byte((secondsSinceMidnightUTC & 0xFF))
	msg[4] = byte((secondsSinceMidnightUTC & 0xFFFF) >> 8)

	// TODO. Number of uplink messages. See p.12.
	// msg[5]
	// msg[6]

	return prepareMessage(msg)
}

func relayMessage(msgtype uint16, msg []byte) {
	ret := make([]byte, len(msg)+4)
	// See p.15.
	ret[0] = byte(msgtype) // Uplink message ID.
	ret[1] = 0x00          //TODO: Time.
	ret[2] = 0x00          //TODO: Time.
	ret[3] = 0x00          //TODO: Time.

	for i := 0; i < len(msg); i++ {
		ret[i+4] = msg[i]
	}

	outConn.Write(prepareMessage(ret))
}

func heartBeatSender() {
	for {
		outConn.Write(makeHeartbeat())
		time.Sleep(1 * time.Second)
	}
}

func updateStatus() {
	t := make([]msg, 0)
	m := len(MsgLog)
	UAT_messages_last_minute := uint(0)
	ES_messages_last_minute := uint(0)
	for i := 0; i < m; i++ {
		if time.Now().Sub(MsgLog[i].TimeReceived).Minutes() < 1 {
			t = append(t, MsgLog[i])
			if MsgLog[i].MessageClass == MSGCLASS_UAT {
				UAT_messages_last_minute++
			} else if MsgLog[i].MessageClass == MSGCLASS_ES {
				ES_messages_last_minute++
			}
		}
	}
	MsgLog = t
	globalStatus.UAT_messages_last_minute = UAT_messages_last_minute
	globalStatus.ES_messages_last_minute = ES_messages_last_minute
}

func parseInput(buf string) ([]byte, uint16) {
	x := strings.Split(buf, ";") // Discard everything after the first ';'.
	if len(x) == 0 {
		return nil, 0
	}
	s := x[0]
	if len(s) == 0 {
		return nil, 0
	}
	msgtype := uint16(0)

	s = s[1:]
	msglen := len(s) / 2

	if len(s)%2 != 0 { // Bad format.
		return nil, 0
	}

	if msglen == UPLINK_FRAME_DATA_BYTES {
		msgtype = MSGTYPE_UPLINK
	} else if msglen == 34 {
		msgtype = MSGTYPE_LONG_REPORT
	} else if msglen == 18 {
		msgtype = MSGTYPE_BASIC_REPORT
	} else {
		msgtype = 0
	}

	if msgtype == 0 {
		fmt.Printf("UNKNOWN MESSAGE TYPE: %s - msglen=%d\n", s, msglen)
	}

	// Now, begin converting the string into a byte array.
	frame := make([]byte, UPLINK_FRAME_DATA_BYTES)
	hex.Decode(frame, []byte(s))

	var thisMsg msg
	thisMsg.MessageClass = MSGCLASS_UAT
	thisMsg.TimeReceived = time.Now()
	thisMsg.Data = frame
	MsgLog = append(MsgLog, thisMsg)
	updateStatus()

	return frame, msgtype
}

type settings struct {
	UAT_Enabled					bool
	ES_Enabled					bool
}

type status struct {
	Version						string
	Devices						uint
	UAT_messages_last_minute	uint
	UAT_messages_max			uint
	ES_messages_last_minute		uint
	ES_messages_max				uint
	GPS_satellites_locked		uint
}

var globalSettings settings
var globalStatus status

func handleManagementConnection(conn net.Conn) {
	defer conn.Close()
	rw := bufio.NewReader(conn)
	for {
		s, err := rw.ReadString('\n')
		if err != nil {
			break
		}
		s = strings.Trim(s, "\r\n")
		if s == "STATUS" {
			resp, _ := json.Marshal(&globalStatus)
			conn.Write(resp)
		} else if s == "SETTINGS" {
			resp, _ := json.Marshal(&globalSettings)
			conn.Write(resp)
		} else if s == "QUIT" {
			break
		} else {
			// Assume settings.
			//TODO: Make this so that there is some positive way of doing this versus assuming that everything other than commands above are settings.
			var newSettings settings
			err := json.Unmarshal([]byte(s), &newSettings)
			if err != nil {
				fmt.Printf("%s - error: %s\n", s, err.Error())
			} else {
				fmt.Printf("new settings: %s\n", s)
				globalSettings = newSettings
				saveSettings()
			}
		}
	}
}

func managementInterface() {
	ln, err := net.Listen("tcp", "127.0.0.1:9110")
	if err != nil { //TODO
		fmt.Printf("couldn't open management port: %s\n", err.Error())
		return
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil { //TODO
			continue
		}
		go handleManagementConnection(conn)
	}
}


func defaultSettings() {
	globalSettings.UAT_Enabled = true //TODO
	globalSettings.ES_Enabled = false //TODO
}

func readSettings() {
	fd, err := os.Open(configLocation)
	defer fd.Close()
	if err != nil {
		fmt.Printf("can't read settings %s: %s\n", configLocation, err.Error())
		defaultSettings()
		return
	}
	buf := make([]byte, 1024)
	count, err := fd.Read(buf)
	if err != nil {
		fmt.Printf("can't read settings %s: %s\n", configLocation, err.Error())
		defaultSettings()
		return
	}
	var newSettings settings
	err = json.Unmarshal(buf[0:count], &newSettings)
	if err != nil {
		fmt.Printf("can't read settings %s: %s\n", configLocation, err.Error())
		defaultSettings()
		return	
	}
	globalSettings = newSettings
	fmt.Printf("read in settings.\n")
}

func saveSettings() {
	fd, err := os.OpenFile(configLocation, os.O_CREATE | os.O_WRONLY, os.FileMode(0644))
	defer fd.Close()
	if err != nil {
		fmt.Printf("can't save settings %s: %s\n", configLocation, err.Error())
		return
	}
	jsonSettings, _ := json.Marshal(&globalSettings)
	fd.Write(jsonSettings)
	fmt.Printf("wrote settings.\n")
}

func main() {
	MsgLog = make([]msg, 0)
	globalStatus.Version = stratuxVersion
	globalStatus.Devices = 123 //TODO
	globalStatus.UAT_messages_last_minute = 567 //TODO
	globalStatus.ES_messages_last_minute = 981 //TODO

	readSettings()

	crcInit() // Initialize CRC16 table.

	// Open UDP port to send out the messages.
	addr, err := net.ResolveUDPAddr("udp", ipadAddr)
	if err != nil {
		panic(err)
	}
	outConn, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		panic("error creating UDP socket: " + err.Error())
	}

	// Start the heartbeat message loop in the background, once per second.
	go heartBeatSender()
	// Start the management interface.
	go managementInterface()

	reader := bufio.NewReader(os.Stdin)

	for {
		buf, _ := reader.ReadString('\n')
		o, msgtype := parseInput(buf)
		if o != nil && msgtype != 0 {
			relayMessage(msgtype, o)
		}
	}

}