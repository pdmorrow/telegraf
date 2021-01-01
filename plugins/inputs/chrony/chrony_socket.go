package chrony

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"time"
)

type ChronySocketType uint8

const (
	CHRONY_SOCKET_UNSPEC = 0
	CHRONY_SOCKET_DOMAIN = 1
	CHRONY_SOCKET_V4UDP  = 2
	CHRONY_SOCKET_V6UDP  = 2

	PROTO_VERSION              byte = 0x6
	REQ_TRACKING_HI_BYTE       byte = 0x0
	REQ_TRACKING_LO_BYTE       byte = 0x21
	PKT_TYPE_CMD_REQUEST       byte = 0x1
	PKT_TYPE_CMD_REPLY         byte = 0x2
	COMMAND_LENGTH_BYTES            = 104
	COMMAND_REPLY_LENGTH_BYTES      = COMMAND_LENGTH_BYTES
)

var (
	// socket_type indicates which connection method we use when not
	// collecting metrics via chronyc.
	socket_type ChronySocketType = CHRONY_SOCKET_UNSPEC

	// Location of the unix domain socket that chronyd listens on.
	server_unix_socket string = "/var/run/chrony/chronyd.sock"

	// v4 UDP server endpoint
	server_v4_addr string = "127.0.0.1:323"

	// v6 UDP server endpoint
	server_v6_addr string = "[::1]:323"
)

// This is taken from UTI_FloatNetworkToHost() in chrony/util.c:
//
// 32-bit floating-point format consisting of 7-bit signed exponent
// and 25-bit signed coefficient without hidden bit. The result is
// calculated as: 2^(exp - 25) * coef.
func bytes_to_float32(floatbytes []byte) (float32, error) {

	const FLOAT_EXP_BITS = 7
	const FLOAT_COEF_BITS = 25

	var exp, coef int32
	var asuint32 uint32
	buf := bytes.NewReader(floatbytes)
	err := binary.Read(buf, binary.BigEndian, &asuint32)
	if err != nil {
		return 0.0, err
	}

	exp = int32(asuint32 >> FLOAT_COEF_BITS)
	if exp >= (1 << (FLOAT_EXP_BITS - 1)) {
		exp -= 1 << FLOAT_EXP_BITS
	}

	exp -= FLOAT_COEF_BITS

	coef = int32(asuint32 % (1 << FLOAT_COEF_BITS))
	if coef >= (1 << (FLOAT_COEF_BITS - 1)) {
		coef -= 1 << FLOAT_COEF_BITS
	}

	return float32(coef) * float32(math.Pow(2.0, float64(exp))), nil
}

// bytes_to_reference_name returns a name that corresponds to the reference
// id. It does this by decoding the ip address of the time sync reference. If
// dns_lookup is true and there is an IP reference then we'll attempt to resolve
// the name of the reference. If there is no IP reference then we'll return the
// passed reference_id as the reference name.
func bytes_to_reference_name(addrbytes []byte,
	reference_id string, dns_lookup bool) string {

	const IPADDR_INET4 = 1
	const IPADDR_INET6 = 2

	addr := reference_id
	af := addrbytes[16:18]
	switch binary.BigEndian.Uint16(af) {
	case IPADDR_INET4:
		addr = net.IPv4(addrbytes[0],
			addrbytes[1],
			addrbytes[2],
			addrbytes[3]).String()
	case IPADDR_INET6:
		addr = net.IP{addrbytes[0],
			addrbytes[1],
			addrbytes[2],
			addrbytes[3],
			addrbytes[4],
			addrbytes[5],
			addrbytes[6],
			addrbytes[7],
			addrbytes[8],
			addrbytes[9],
			addrbytes[10],
			addrbytes[11],
			addrbytes[12],
			addrbytes[13],
			addrbytes[14],
			addrbytes[15]}.String()
	}

	if dns_lookup && addr != reference_id {
		names, err := net.LookupAddr(addr)
		if err == nil {
			// The resolved name (correctly) ends with a ".", chop it off
			// for display purposes.
			return names[0][:len(names[0])-len(".")]
		}
	}

	return addr
}

// bytes_to_leap_status decodes leap_status_bytes and returns a human readable
// string version.
func bytes_to_leap_status(leap_status_bytes []byte) string {

	const LEAP_Normal = 0
	const LEAP_InsertSecond = 1
	const LEAP_DeleteSecond = 2
	const LEAP_Unsynchronised = 3

	switch binary.BigEndian.Uint16(leap_status_bytes) {
	case LEAP_Normal:
		return "normal"
	case LEAP_InsertSecond:
		return "insert second"
	case LEAP_DeleteSecond:
		return "delete second"
	case LEAP_Unsynchronised:
		return "not synchronised"
	default:
		return "invalid"
	}
}

// openSocket opens a connection to chronyd. To match the behaviour of chronyc
// we attempt to open connection types in the following order (and will return
// on first success):
//
// 1) Domain socket.
// 2) IPv4 local UDP socket.
// 3) IPv6 local UDP socket.
//
// The first time this function is called the parameter socket_type should be
// set to CHRONY_SOCKET_UNSPEC. This forces the function to attempt to connect
// via all means. For the first connection type that succeeds a net.Conn along
// with a ChronySocketType is returned. The caller should then for subsequent
// calls to openSocket pass the previously returned ChronySocketType value.
// This means that the first successful connection type will be reused for the
// lifetime of the plugin.
func openSocket(socket_type ChronySocketType) (net.Conn, ChronySocketType, error) {
	// Attempt the initial connection or subsequent connection via unix domain
	// socket.
	if socket_type == CHRONY_SOCKET_UNSPEC ||
		socket_type == CHRONY_SOCKET_DOMAIN {

		chronyd_sock := &net.UnixAddr{
			Name: server_unix_socket,
			Net:  "unixgram",
		}

		client_sock := &net.UnixAddr{
			Name: fmt.Sprintf("/var/run/telegraf-chrony-%d.sock", os.Getpid()),
			Net:  "unixgram",
		}

		os.Remove(client_sock.Name)
		conn, err := net.DialUnix("unixgram", client_sock, chronyd_sock)
		if err != nil {
			if socket_type == CHRONY_SOCKET_UNSPEC {
				// This is the initial connection attempt, try v4 next.
				goto try_v4
			} else {
				fmt.Printf("using domain sock\n")
				return nil, CHRONY_SOCKET_DOMAIN, err
			}
		} else {
			// Allow chronyd running as non root to write to the socket.
			os.Chmod(client_sock.Name, 0666)

			// We'll use a unix domain socket for the life of the plugin.
			return conn, CHRONY_SOCKET_DOMAIN, nil
		}
	}

try_v4:

	// Initial connection attempt when the domain socket connection failed, or
	// subsequent connection using ipv4 udp.
	if socket_type == CHRONY_SOCKET_UNSPEC ||
		socket_type == CHRONY_SOCKET_V4UDP {
		conn, err := net.Dial("udp4", server_v4_addr)
		if err != nil {
			if socket_type == CHRONY_SOCKET_UNSPEC {
				// This is the intial connection attempt, try v6 next.
				goto try_v6
			} else {
				return nil, CHRONY_SOCKET_V4UDP, err
			}
		} else {
			// We'll use a v4 udp socket for the life of the plugin.
			return conn, CHRONY_SOCKET_V4UDP, nil
		}
	}

try_v6:

	// Initial connection attempt when the domain socket connection failed, or
	// subsequent connection using ipv6 udp.
	if socket_type == CHRONY_SOCKET_UNSPEC ||
		socket_type == CHRONY_SOCKET_V6UDP {
		conn, err := net.Dial("udp6", server_v6_addr)
		if err != nil {
			// Nothing worked, return the requested socket type again and
			// the next time Gather is called we'll try the whole process
			// again.
			return nil, socket_type, err
		} else {
			// We'll use a v6 udp socket for the life of the plugin.
			return conn, CHRONY_SOCKET_V6UDP, nil
		}
	}

	return nil, CHRONY_SOCKET_UNSPEC, fmt.Errorf("unexpected code path")
}

// trackingFromDomainSocket reads tracking information from chronyd via a
// socket connection. It bundles all tags and fields and returns them to be
// handed over to the accumlator.
//
// This function is effectively socket based version of processChronycOutput().
func (c *Chrony) trackingFromDomainSocket() (map[string]interface{}, map[string]string, error) {
	tags := map[string]string{}
	fields := map[string]interface{}{}

	var conn net.Conn
	var err error
	conn, socket_type, err = openSocket(socket_type)
	if err != nil {
		return nil, nil, err
	}

	defer conn.Close()

	// Construct a request payload.
	pkt := [COMMAND_LENGTH_BYTES]byte{PROTO_VERSION, // version
		PKT_TYPE_CMD_REQUEST,                       // pkt_type
		0x0,                                        // res1
		0x0,                                        // res2
		REQ_TRACKING_HI_BYTE, REQ_TRACKING_LO_BYTE} // command

	// Set a random 4 byte sequence number.
	rand.Seed(time.Now().UnixNano())
	rand.Read(pkt[8:12])

	// Write the command to chronyd.
	conn.SetWriteDeadline(time.Now().Add(time.Second * 2))
	_, err = conn.Write(pkt[0:])
	if err != nil {
		return nil, nil, err
	}

	// Read the reply and parse out the data.
	reply := make([]byte, COMMAND_REPLY_LENGTH_BYTES)
	conn.SetReadDeadline(time.Now().Add(time.Second * 2))
	_, err = conn.Read(reply)
	if err != nil {
		return nil, nil, err
	}

	// Check the reply is valid.
	version := reply[0]
	pkt_type := reply[1]
	res1 := reply[2]
	res2 := reply[3]
	seq_bytes := reply[16:20]
	cmd_bytes := reply[4:6]

	if version != PROTO_VERSION {
		return nil, nil, fmt.Errorf("version mismatch, got %x expected %x\n", version, PROTO_VERSION)
	}

	if pkt_type != PKT_TYPE_CMD_REPLY {
		return nil, nil, fmt.Errorf("pkt_type mismatch, got %x expected %x\n", pkt_type, PKT_TYPE_CMD_REPLY)
	}

	if res1 != 0 || res2 != 0 {
		return nil, nil, fmt.Errorf("unexpected resN codes in reply: %x, %x\n", res1, res2)
	}

	if bytes.Compare(pkt[8:12], seq_bytes) != 0 {
		return nil, nil, fmt.Errorf("unexpected sequence number in reply\n")
	}

	if bytes.Compare(pkt[4:6], cmd_bytes) != 0 {
		return nil, nil, fmt.Errorf("unexpected command in reply\n")
	}

	// Skip the packet header & padding to get the tracking data.
	tracking_data := reply[28:]

	// Add tags first of all, these are:
	//
	// reference_id.
	// reference_name (N.B. when screen scraping via chronyc this tag will not
	//                 be present).
	// stratum.
	// leap_status.
	reference_id_bytes := tracking_data[0:4]
	reference_id := binary.BigEndian.Uint32(reference_id_bytes)
	reference_id_str := fmt.Sprintf("%X", reference_id)
	tags["reference_id"] = reference_id_str

	ip_addr_bytes := tracking_data[4:24]
	tags["reference_name"] = bytes_to_reference_name(ip_addr_bytes,
		reference_id_str, c.DNSLookup)

	stratum_bytes := tracking_data[24:26]
	stratum := binary.BigEndian.Uint16(stratum_bytes)
	tags["stratum"] = fmt.Sprintf("%d", stratum)

	leap_status_bytes := tracking_data[26:28]
	tags["leap_status"] = bytes_to_leap_status(leap_status_bytes)

	// The rest are fields, N.B. Ref time is skipped.
	current_correction_bytes := tracking_data[40:44]
	system_time, err := bytes_to_float32(current_correction_bytes)
	if err != nil {
		return nil, nil, err
	}
	if system_time > 0.0 {
		system_time = -system_time
	} else {
		system_time = float32(math.Abs(float64(system_time)))
	}
	fields["system_time"] = system_time

	last_offset_bytes := tracking_data[44:48]
	fields["last_offset"], err = bytes_to_float32(last_offset_bytes)
	if err != nil {
		return nil, nil, err
	}

	rms_offset_bytes := tracking_data[48:52]
	fields["rms_offset"], err = bytes_to_float32(rms_offset_bytes)
	if err != nil {
		return nil, nil, err
	}

	freq_ppm_bytes := tracking_data[52:56]
	fields["frequency"], err = bytes_to_float32(freq_ppm_bytes)
	if err != nil {
		return nil, nil, err
	}

	resid_freq_ppm_bytes := tracking_data[56:60]
	fields["residual_freq"], err = bytes_to_float32(resid_freq_ppm_bytes)
	if err != nil {
		return nil, nil, err
	}

	skew_ppm_bytes := tracking_data[60:64]
	fields["skew"], err = bytes_to_float32(skew_ppm_bytes)
	if err != nil {
		return nil, nil, err
	}

	root_delay_bytes := tracking_data[64:68]
	fields["root_delay"], err = bytes_to_float32(root_delay_bytes)
	if err != nil {
		return nil, nil, err
	}

	root_dispersion_bytes := tracking_data[68:72]
	fields["root_dispersion"], err = bytes_to_float32(root_dispersion_bytes)
	if err != nil {
		return nil, nil, err
	}

	last_update_interval_bytes := tracking_data[72:76]
	fields["update_interval"], err = bytes_to_float32(last_update_interval_bytes)
	if err != nil {
		return nil, nil, err
	}

	return fields, tags, nil
}
