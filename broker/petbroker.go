//go:debug tlsrsakex=1

// petbroker — PetMarvel C1 猫砂盆本地 broker
//
// 端口：
//   :1883  TLS  — 设备接入（自签 RSA 证书；实测设备不校验证书）
//   :1884  TCP  — HA 接入（普通 MQTT 3.1.1）
//
// 数据流：
//   设备 up_raw  --TLV解析-->  状态表  --JSON-->  HA (petmarvel/state)
//   HA (petmarvel/cmd)  --JSON-->  down_raw TLV  -->  设备
package main

import (
	"bufio"
	"crypto/rsa"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- MQTT 常量 ----------
const (
	pktCONNECT     = 1
	pktCONNACK     = 2
	pktPUBLISH     = 3
	pktPUBACK      = 4
	pktSUBSCRIBE   = 8
	pktSUBACK      = 9
	pktPINGREQ     = 12
	pktPINGRESP    = 13
	pktDISCONNECT  = 14
	pktUNSUBSCRIBE = 10
)

// ---------- TLV 字段 ID ----------
const (
	fAction      = 0x01 // 动作/事件码
	fAutoClean   = 0x02
	fCatPresent  = 0x04
	fLight       = 0x05
	fSound       = 0x06
	fLock        = 0x07
	fCatUse      = 0x09 // [A][体重g][时长s]
	fDelay       = 0x0a
	fResetDone   = 0x0b
	fApproach    = 0x0d
	fWorkStatus  = 0x11
	fActionID    = 0x13
	fSmallCat    = 0x14
	fDeepClean   = 0x15
	fLid         = 0x16 // 00=在位 01=取下
	fBin         = 0x17 // 00=在位 01=抽出
	fBinFull     = 0x18 // 00=正常 01=满
	fErrorCode   = 0x23
	fVersion     = 0x64
	fLastUsage   = 0x65 // "YYYY-MM-DD HH:MM:SS N"
	fEventFlag   = 0x80
)

var workStatusText = map[byte]string{
	0: "idle", 1: "cleaning", 2: "cleaning_complete",
	3: "dumping", 4: "dumping_complete",
	5: "resetting", 6: "resetting_complete",
	7: "paused", 8: "cat_approaching", 9: "cat_entering",
}

// ---------- 全局状态 ----------
// deviceSession 一台猫砂盆的运行期状态（按 deviceName 区分）
type deviceSession struct {
	mu       sync.Mutex
	conn     net.Conn
	clientID string
	product  string
	deviceID string
	tlv      map[byte][]byte
	lastSeen time.Time
	ip       string
}

type Broker struct {
	mu      sync.RWMutex
	devices map[string]*deviceSession // key = deviceName
	lastID  string                    // 最后活跃设备，用于单设备 topic 兼容

	haMu    sync.Mutex
	haConns map[*haClient]struct{}
}

type haClient struct {
	conn net.Conn
	mu   sync.Mutex
	subs []string
}

func newBroker() *Broker {
	return &Broker{devices: map[string]*deviceSession{}, haConns: map[*haClient]struct{}{}}
}

func (b *Broker) getSession(id string) *deviceSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.devices[id]
}

func (b *Broker) sessionList() []*deviceSession {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*deviceSession, 0, len(b.devices))
	for _, s := range b.devices {
		out = append(out, s)
	}
	return out
}

// ---------- MQTT 编解码 ----------
func readPacket(r *bufio.Reader) (byte, byte, []byte, error) {
	h, err := r.ReadByte()
	if err != nil {
		return 0, 0, nil, err
	}
	typ := h >> 4
	flags := h & 0x0f
	// remaining length varint
	var rl int
	var mult = 1
	for i := 0; ; i++ {
		if i > 3 {
			return 0, 0, nil, fmt.Errorf("bad remaining length")
		}
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, nil, err
		}
		rl += int(b&0x7f) * mult
		if b&0x80 == 0 {
			break
		}
		mult *= 128
	}
	payload := make([]byte, rl)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, nil, err
	}
	return typ, flags, payload, nil
}

func encodeRemaining(n int) []byte {
	var out []byte
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

func writePacket(w io.Writer, typ byte, flags byte, payload []byte) error {
	buf := make([]byte, 0, len(payload)+6)
	buf = append(buf, typ<<4|flags)
	buf = append(buf, encodeRemaining(len(payload))...)
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// MQTT string: 2字节长度 + UTF8
func mqttString(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

func readMQTTString(b []byte, off int) (string, int) {
	if off+2 > len(b) {
		return "", off
	}
	l := int(binary.BigEndian.Uint16(b[off:]))
	off += 2
	if off+l > len(b) {
		return "", off
	}
	return string(b[off : off+l]), off + l
}

// ---------- TLV ----------
func tlvParse(b []byte) map[byte][]byte {
	out := map[byte][]byte{}
	i := 0
	for i+2 <= len(b) {
		id := b[i]
		l := int(b[i+1])
		if i+2+l > len(b) {
			break
		}
		v := make([]byte, l)
		copy(v, b[i+2:i+2+l])
		out[id] = v
		i += 2 + l
	}
	return out
}

func tlvBuild(id byte, val []byte) []byte {
	out := make([]byte, 0, len(val)+2)
	out = append(out, id, byte(len(val)))
	out = append(out, val...)
	return out
}

func le16(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	return int(binary.LittleEndian.Uint16(b))
}

// ---------- 状态快照 ----------
func (s *deviceSession) snapshotMap() map[string]any {
	s.mu.Lock()
	t := make(map[byte][]byte, len(s.tlv))
	for k, v := range s.tlv {
		t[k] = v
	}
	deviceID, product, lastSeen, ip := s.deviceID, s.product, s.lastSeen, s.ip
	s.mu.Unlock()

	byteVal := func(id byte) (int, bool) {
		v, ok := t[id]
		if !ok || len(v) == 0 {
			return 0, false
		}
		return int(v[0]), true
	}

	out := map[string]any{
		"online":    time.Since(lastSeen) < 3*time.Minute,
		"device_id": deviceID,
		"product":   product,
		"ip":        ip,
		"last_seen": lastSeen.UTC().Format(time.RFC3339),
	}

	if v, ok := byteVal(fWorkStatus); ok {
		out["work_status"] = v
		if s, ok2 := workStatusText[byte(v)]; ok2 {
			out["status_text"] = s
		}
	}
	if v, ok := byteVal(fAutoClean); ok {
		out["auto_clean"] = v
	}
	if v, ok := byteVal(fLight); ok {
		out["light"] = v
	}
	if v, ok := byteVal(fSound); ok {
		out["sound"] = v
	}
	if v, ok := byteVal(fLock); ok {
		out["lock"] = v
	}
	if v, ok := byteVal(fSmallCat); ok {
		out["small_cat_mode"] = v
	}
	if v, ok := byteVal(fDeepClean); ok {
		out["deep_clean"] = v
	}
	if v, ok := byteVal(fDelay); ok {
		out["delay_minutes"] = v
	}
	if v, ok := byteVal(fLid); ok {
		out["lid_installed"] = v == 0
	}
	if v, ok := byteVal(fBin); ok {
		out["bin_inserted"] = v == 0
	}
	if v, ok := byteVal(fBinFull); ok {
		out["bin_full"] = v == 1
	}
	if v, ok := byteVal(fCatPresent); ok {
		out["cat_present"] = v == 1
	}
	if v, ok := t[fCatUse]; ok && len(v) >= 6 {
		out["cat_use"] = map[string]any{
			"lead":       le16(v[0:2]),
			"weight_g":   le16(v[2:4]),
			"duration_s": le16(v[4:6]),
		}
	}
	if v, ok := t[fLastUsage]; ok {
		// 0x65 有两种语义：全量帧里是秒偏移（如 "28800"），
		// 使用记录帧里才是 "YYYY-MM-DD HH:MM:SS N" 时间串。
		s := string(v)
		if strings.Contains(s, ":") {
			out["last_usage"] = s
		} else {
			out["last_usage_raw"] = s
		}
	}
	if v, ok := t[fVersion]; ok {
		out["fw_version"] = string(v)
	}
	if v, ok := byteVal(fAction); ok {
		out["last_action_code"] = v
	}
	if v, ok := byteVal(fErrorCode); ok {
		out["error_code"] = v
	}

	// 原始字段，便于调试
	raw := map[string]string{}
	for k, v := range t {
		raw[fmt.Sprintf("0x%02x", k)] = fmt.Sprintf("%x", v)
	}
	out["raw"] = raw
	return out
}

// ---------- 广播到 HA ----------
func (b *Broker) publishHA(topic string, payload []byte, retain bool) {
	b.haMu.Lock()
	targets := make([]*haClient, 0, len(b.haConns))
	for c := range b.haConns {
		targets = append(targets, c)
	}
	b.haMu.Unlock()

	for _, c := range targets {
		if !c.matches(topic) {
			continue
		}
		hdr := make([]byte, 0, 2+len(topic))
		hdr = append(hdr, mqttString(topic)...)
		msg := append(hdr, payload...)
		fl := byte(0)
		if retain {
			fl |= 0x01
		}
		c.mu.Lock()
		err := writePacket(c.conn, pktPUBLISH, fl, msg)
		c.mu.Unlock()
		if err != nil {
			c.close()
		}
	}
}

func (c *haClient) matches(topic string) bool {
	for _, f := range c.subs {
		if f == "#" || f == topic {
			return true
		}
		if strings.HasSuffix(f, "/#") && strings.HasPrefix(topic, strings.TrimSuffix(f, "/#")) {
			return true
		}
		if strings.HasSuffix(f, "/+") {
			pre := strings.TrimSuffix(f, "/+")
			if strings.HasPrefix(topic, pre+"/") && !strings.Contains(topic[len(pre)+1:], "/") {
				return true
			}
		}
	}
	return false
}

func (c *haClient) close() {
	c.conn.Close()
}

// publishRetained 直接给这个客户端发一条 retain 消息（跳过订阅匹配）
func (c *haClient) publishRetained(topic string, payload []byte) {
	msg := append(mqttString(topic), payload...)
	c.mu.Lock()
	err := writePacket(c.conn, pktPUBLISH, 0x01, msg)
	c.mu.Unlock()
	if err != nil {
		c.close()
	}
}

// ---------- 推给设备 ----------
func (s *deviceSession) publishTopic(topic string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("device offline")
	}
	msg := append(mqttString(topic), payload...)
	s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err := writePacket(s.conn, pktPUBLISH, 0, msg)
	s.conn.SetWriteDeadline(time.Time{})
	return err
}

func (s *deviceSession) publishDownRaw(payload []byte) error {
	topic := fmt.Sprintf("/sys/%s/%s/thing/model/down_raw", s.product, s.deviceID)
	return s.publishTopic(topic, payload)
}

// ---------- 命令表 ----------
func buildCommand(name string, args map[string]any) ([]byte, error) {
	getInt := func(k string) (byte, bool) {
		v, ok := args[k]
		if !ok {
			return 0, false
		}
		switch t := v.(type) {
		case float64:
			return byte(t), true
		case bool:
			if t {
				return 1, true
			}
			return 0, true
		case string:
			n, err := strconv.Atoi(t)
			if err == nil {
				return byte(n), true
			}
		}
		return 0, false
	}
	switch name {
	case "clean":
		return []byte{fAction, 0x01, 0x01}, nil
	case "level":
		return []byte{fAction, 0x01, 0x04}, nil
	case "dump":
		return []byte{fAction, 0x01, 0x03}, nil
	case "auto_clean":
		v, _ := getInt("value")
		return tlvBuild(fAutoClean, []byte{v}), nil
	case "light":
		v, _ := getInt("value")
		return tlvBuild(fLight, []byte{v}), nil
	case "sound":
		v, _ := getInt("value")
		return tlvBuild(fSound, []byte{v}), nil
	case "lock":
		v, _ := getInt("value")
		return tlvBuild(fLock, []byte{v}), nil
	case "small_cat_mode":
		v, _ := getInt("value")
		return tlvBuild(fSmallCat, []byte{v}), nil
	case "deep_clean":
		v, _ := getInt("value")
		return tlvBuild(fDeepClean, []byte{v}), nil
	case "delay_minutes":
		v, _ := getInt("value")
		return tlvBuild(fDelay, []byte{v}), nil
	}
	return nil, fmt.Errorf("unknown command: %s", name)
}

// ---------- 设备连接处理 ----------
// parseClientID 从 a14KPydEhd3.001122334455|securemode=... 拆出 productKey / deviceName
func parseClientID(cid string) (product, deviceID string) {
	head := cid
	if i := strings.Index(head, "|"); i > 0 {
		head = head[:i]
	}
	if j := strings.Index(head, "."); j > 0 {
		return head[:j], head[j+1:]
	}
	return "", ""
}

// upsertSession 按 deviceName 建/更新会话；同一设备重复连接会踢掉旧连接
func (b *Broker) upsertSession(deviceID, product, cid string, conn net.Conn, ip string) *deviceSession {
	b.mu.Lock()
	s, ok := b.devices[deviceID]
	if !ok {
		s = &deviceSession{deviceID: deviceID, tlv: map[byte][]byte{}}
		b.devices[deviceID] = s
	}
	b.lastID = deviceID
	b.mu.Unlock()

	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.clientID = cid
	s.product = product
	s.lastSeen = time.Now()
	s.ip = ip
	s.mu.Unlock()

	if old != nil && old != conn {
		old.Close()
	}
	return s
}

func (b *Broker) handleDevice(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	host, _, _ := net.SplitHostPort(remote)
	log.Printf("[device] connected from %s", remote)
	r := bufio.NewReaderSize(conn, 8192)

	var sess *deviceSession
	defer func() {
		if sess != nil {
			sess.mu.Lock()
			if sess.conn == conn {
				sess.conn = nil
			}
			sess.lastSeen = time.Now()
			sess.mu.Unlock()
			b.emitState(sess)
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		typ, _, payload, err := readPacket(r)
		if err != nil {
			log.Printf("[device] read closed: %v", err)
			return
		}
		switch typ {
		case pktCONNECT:
			// CONNECT 可变头：protoLen(2)+proto+level(1)+flags(1)+keepalive(2) = 10 字节
			cid, _ := readMQTTString(payload, 10)
			product, deviceID := parseClientID(cid)
			if deviceID == "" {
				log.Printf("[device] CONNECT 无法解析 deviceName，clientId=%s", cid)
				deviceID = "unknown-" + strings.ReplaceAll(host, ".", "_")
			}
			sess = b.upsertSession(deviceID, product, cid, conn, host)
			log.Printf("[device] CONNECT clientId=%s -> device=%s product=%s", cid, deviceID, product)
			if err := writePacket(conn, pktCONNACK, 0, []byte{0x00, 0x00}); err != nil {
				return
			}
			b.emitState(sess)

		case pktPUBLISH:
			topic, off := readMQTTString(payload, 0)
			// QoS 0 无 packetId；本设备固定 qos0
			body := payload[off:]
			b.handleDevicePublish(sess, conn, topic, body)

		case pktPINGREQ:
			if err := writePacket(conn, pktPINGRESP, 0, nil); err != nil {
				return
			}
			if sess != nil {
				sess.mu.Lock()
				sess.lastSeen = time.Now()
				sess.mu.Unlock()
			}

		case pktSUBSCRIBE:
			// 设备理论上不订阅；回 SUBACK 以防万一
			if len(payload) >= 2 {
				pid := payload[0:2]
				codes := make([]byte, (len(payload)-2)/3)
				for i := range codes {
					codes[i] = 0x00
				}
				writePacket(conn, pktSUBACK, 0, append(pid, codes...))
			}

		case pktDISCONNECT:
			log.Printf("[device] DISCONNECT")
			return
		}
	}
}

func (b *Broker) handleDevicePublish(sess *deviceSession, conn net.Conn, topic string, body []byte) {
	if sess != nil {
		sess.mu.Lock()
		sess.lastSeen = time.Now()
		sess.mu.Unlock()
	}

	// 回给设备：优先走会话连接（带写锁），无会话时退回裸连接
	reply := func(t string, payload []byte) error {
		if sess != nil {
			return sess.publishTopic(t, payload)
		}
		msg := append(mqttString(t), payload...)
		return writePacket(conn, pktPUBLISH, 0, msg)
	}

	if strings.HasSuffix(topic, "/up_raw") {
		fields := tlvParse(body)
		if sess != nil {
			sess.mu.Lock()
			for k, v := range fields {
				sess.tlv[k] = v
			}
			sess.mu.Unlock()
		}

		log.Printf("[device] up_raw %x", body)
		// 回执，与阿里云行为一致
		replyTopic := strings.Replace(topic, "/up_raw", "/up_raw_reply", 1)
		if err := reply(replyTopic, []byte("report reply")); err != nil {
			log.Printf("[device] up_raw_reply failed: %v", err)
		}

		b.emitState(sess)
		return
	}
	if strings.HasSuffix(topic, "/up_raw_reply") {
		return
	}
	// NTP 校时：设备重启后会请求，必须回，否则时间漂移
	if strings.Contains(topic, "/ext/ntp/") && strings.HasSuffix(topic, "/request") {
		log.Printf("[ntp] request %s %s", topic, string(body))
		var req struct {
			DeviceSendTime string `json:"deviceSendTime"`
		}
		_ = json.Unmarshal(body, &req)
		now := time.Now().UnixMilli()
		resp := map[string]string{
			"deviceSendTime": req.DeviceSendTime,
			"serverSendTime": strconv.FormatInt(now, 10),
			"serverRecvTime": strconv.FormatInt(now, 10),
		}
		if js, err := json.Marshal(resp); err == nil {
			respTopic := strings.Replace(topic, "/request", "/response", 1)
			if err := reply(respTopic, js); err != nil {
				log.Printf("[ntp] reply failed: %v", err)
			} else {
				log.Printf("[ntp] replied %s -> %s", respTopic, string(js))
			}
		}
		return
	}
	log.Printf("[device] publish %s %x", topic, body)
}

// 把单设备状态 + 设备清单发到 HA
func (b *Broker) emitState(s *deviceSession) {
	if s == nil {
		return
	}
	snap := s.snapshotMap()
	id, _ := snap["device_id"].(string)
	if id == "" {
		id = "unknown"
	}
	if js, err := json.Marshal(snap); err == nil {
		b.publishHA("petmarvel/"+id+"/state", js, true)
		if b.deviceCount() == 1 {
			// 单设备时同时保留老 topic，兼容旧配置
			b.publishHA("petmarvel/state", js, true)
		}
	}
	b.publishDeviceList()
}

func (b *Broker) deviceCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.devices)
}

// 设备清单：HA 侧据此自动列出本地 broker 上的所有猫砂盆
func (b *Broker) publishDeviceList() {
	list := []map[string]any{}
	for _, s := range b.sessionList() {
		snap := s.snapshotMap()
		list = append(list, map[string]any{
			"device_id":  snap["device_id"],
			"product":    snap["product"],
			"ip":         snap["ip"],
			"online":     snap["online"],
			"last_seen":  snap["last_seen"],
			"fw_version": snap["fw_version"],
		})
	}
	if js, err := json.Marshal(list); err == nil {
		b.publishHA("petmarvel/devices", js, true)
	}
}

// ---------- HA 连接处理 ----------
func (b *Broker) handleHA(conn net.Conn) {
	c := &haClient{conn: conn}
	defer func() {
		b.haMu.Lock()
		delete(b.haConns, c)
		b.haMu.Unlock()
		conn.Close()
		log.Printf("[ha] disconnected %s", conn.RemoteAddr())
	}()

	r := bufio.NewReaderSize(conn, 8192)
	for {
		typ, _, payload, err := readPacket(r)
		if err != nil {
			return
		}
		switch typ {
		case pktCONNECT:
			b.haMu.Lock()
			b.haConns[c] = struct{}{}
			b.haMu.Unlock()
			writePacket(conn, pktCONNACK, 0, []byte{0x00, 0x00})
			log.Printf("[ha] connected %s", conn.RemoteAddr())
			c.mu.Lock()
			c.subs = []string{"petmarvel/#"}
			c.mu.Unlock()
			// 立刻推一份当前所有设备状态
			b.pushStateTo(c)

		case pktSUBSCRIBE:
			if len(payload) < 2 {
				continue
			}
			pid := payload[0:2]
			off := 2
			var granted []byte
			for off < len(payload) {
				f, next := readMQTTString(payload, off)
				if next == off {
					break
				}
				off = next
				if off < len(payload) {
					off++ // qos byte
				}
				c.mu.Lock()
				c.subs = append(c.subs, f)
				c.mu.Unlock()
				granted = append(granted, 0x00)
				log.Printf("[ha] subscribe %s", f)
			}
			writePacket(conn, pktSUBACK, 0, append(pid, granted...))
			b.pushStateTo(c)

		case pktPUBLISH:
			topic, off := readMQTTString(payload, 0)
			body := payload[off:]
			b.handleHAPublish(topic, body)

		case pktPINGREQ:
			writePacket(conn, pktPINGRESP, 0, nil)

		case pktDISCONNECT:
			return
		}
	}
}

func (b *Broker) handleHAPublish(topic string, body []byte) {
	log.Printf("[ha] cmd %s %s", topic, string(body))
	// 两种 topic：petmarvel/cmd（最后活跃设备）和 petmarvel/<device_id>/cmd
	var deviceID string
	switch {
	case topic == "petmarvel/cmd":
		deviceID = ""
	case strings.HasPrefix(topic, "petmarvel/") && strings.HasSuffix(topic, "/cmd"):
		deviceID = strings.TrimSuffix(strings.TrimPrefix(topic, "petmarvel/"), "/cmd")
	default:
		return
	}
	var req struct {
		Cmd   string         `json:"cmd"`
		Value any            `json:"value"`
		Args  map[string]any `json:"args"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[ha] bad json: %v", err)
		return
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	if req.Value != nil {
		args["value"] = req.Value
	}
	payload, err := buildCommand(req.Cmd, args)
	if err != nil {
		log.Printf("[ha] %v", err)
		return
	}

	var sess *deviceSession
	if deviceID != "" {
		sess = b.getSession(deviceID)
	} else {
		b.mu.RLock()
		if b.lastID != "" {
			sess = b.devices[b.lastID]
		}
		b.mu.RUnlock()
	}
	if sess == nil {
		log.Printf("[ha] 没有可用设备（topic=%s）", topic)
		b.publishHA("petmarvel/cmd_ack", []byte(fmt.Sprintf(`{"cmd":%q,"sent":false,"error":"no_device"}`, req.Cmd)), false)
		return
	}
	if err := sess.publishDownRaw(payload); err != nil {
		log.Printf("[ha] send to device failed: %v", err)
		b.publishHA("petmarvel/cmd_ack", []byte(fmt.Sprintf(`{"cmd":%q,"sent":false}`, req.Cmd)), false)
		return
	}
	b.publishHA("petmarvel/cmd_ack", []byte(fmt.Sprintf(`{"cmd":%q,"device_id":%q,"sent":true}`, req.Cmd, sess.deviceID)), false)
}

// pushStateTo 把当前所有设备的状态 + 设备清单推给一个 HA 客户端
func (b *Broker) pushStateTo(c *haClient) {
	sessions := b.sessionList()
	for _, s := range sessions {
		snap := s.snapshotMap()
		id, _ := snap["device_id"].(string)
		if id == "" {
			id = "unknown"
		}
		if js, err := json.Marshal(snap); err == nil {
			c.publishRetained("petmarvel/"+id+"/state", js)
			if len(sessions) == 1 {
				c.publishRetained("petmarvel/state", js)
			}
		}
	}
	list := []map[string]any{}
	for _, s := range sessions {
		snap := s.snapshotMap()
		list = append(list, map[string]any{
			"device_id":  snap["device_id"],
			"product":    snap["product"],
			"ip":         snap["ip"],
			"online":     snap["online"],
			"last_seen":  snap["last_seen"],
			"fw_version": snap["fw_version"],
		})
	}
	if js, err := json.Marshal(list); err == nil {
		c.publishRetained("petmarvel/devices", js)
	}
}

// ---------- 假 HTTPS 服务端 ----------
// 设备会连 *.link.aliyuncs.com:443 做激活 / OTA 检查 / 日志上报。
// 这里全部接收并回 200，让它认为请求成功，从此不再尝试出网。
// 请求内容写进日志，需要时可以按路径定制响应。
func startFakeHTTPS(addr string, cert tls.Certificate) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16384))
		log.Printf("[fake-https] %s %s host=%s ua=%q body=%s",
			r.Method, r.URL.RequestURI(), r.Host, r.UserAgent(), string(body))

		w.Header().Set("Content-Type", "application/json")

		// /auth/bootstrap：设备启动时靠它换 MQTT 接入凭证（iotToken）。
		// 只回 200 空 JSON 的话设备永远不连 MQTT，表现就是白灯闪 + 每分钟重试。
		// broker 自己不校验 token，所以发一个固定值即可。
		if strings.Contains(r.URL.Path, "/auth/bootstrap") {
			vals, _ := url.ParseQuery(string(body))
			pk := vals.Get("productKey")
			dn := vals.Get("deviceName")
			resp := map[string]any{
				"code":    200,
				"message": "success",
				"data": map[string]any{
					"iotId":    dn,
					"iotToken": "00000000000000000000000000000000",
					"resources": map[string]any{
						"mqtt": map[string]any{
							"host":    "public.iot-as-mqtt.cn-shanghai.aliyuncs.com",
							"port":    1883,
							"version": "1.0",
						},
					},
				},
			}
			if pk != "" || dn != "" {
				log.Printf("[fake-https] bootstrap %s.%s -> issuing local token", pk, dn)
			}
			js, _ := json.Marshal(resp)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(js)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"message":"success","success":true,"data":{}}`))
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// TLS1.2 同时保留设备可能只支持的 RSA 套件和现代客户端用的 ECDHE 套件；
			// 只列 RSA 会让 Go 的 HTTP/2 校验直接失败。
			CipherSuites: []uint16{
				0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
				0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
				0x003c, // TLS_RSA_WITH_AES_128_CBC_SHA256
				0x003d, // TLS_RSA_WITH_AES_256_CBC_SHA256
				0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
				0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
			},
		},
		ReadHeaderTimeout: 10 * time.Second,
		// IoT 设备只讲 HTTP/1.1，直接关掉 h2 避免额外的 ALPN 要求
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	go func() {
		log.Printf("fake HTTPS listening on %s", addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Printf("[fake-https] err: %v", err)
		}
	}()
}

// ---------- 证书 ----------
func loadOrCreateCert(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, "broker.crt")
	keyPath := filepath.Join(dir, "broker.key")
	if _, err := os.Stat(certPath); err == nil {
		if _, err2 := os.Stat(keyPath); err2 == nil {
			return tls.LoadX509KeyPair(certPath, keyPath)
		}
	}
	// 优先复用已有 rsa.crt/rsa.key
	altCert := filepath.Join(dir, "rsa.crt")
	altKey := filepath.Join(dir, "rsa.key")
	if _, err := os.Stat(altCert); err == nil {
		if kg, err2 := tls.LoadX509KeyPair(altCert, altKey); err2 == nil {
			os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kg.Certificate[0]}), 0644)
			return kg, nil
		}
	}
	return tls.Certificate{}, fmt.Errorf("no cert found in %s (need broker.crt/broker.key or rsa.crt/rsa.key)", dir)
}

// ---------- main ----------
func main() {
	var (
		dir     = flag.String("dir", ".", "证书目录")
		devAddr = flag.String("dev", ":1883", "设备 TLS 监听地址")
		haAddr  = flag.String("ha", ":1884", "HA 明文 MQTT 监听地址")
		httpsAddr = flag.String("https", ":8443", "假 HTTPS 服务端监听地址（劫持设备 443 用，空则关闭）")
		logFile = flag.String("log", "", "日志文件（空则写 stdout）")
		verbose = flag.Bool("v", true, "打印帧日志")
	)
	flag.Parse()
	_ = verbose

	if *logFile != "" {
		if f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(f)
		}
	}

	b := newBroker()

	cert, err := loadOrCreateCert(*dir)
	if err != nil {
		log.Fatalf("cert: %v", err)
	}

	// 设备侧：TLS 1.2 + RSA 密钥交换套件（设备 ClientHello 只含 002f/0035/003c/003d）
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
			0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
			0x003c, // TLS_RSA_WITH_AES_128_CBC_SHA256
			0x003d, // TLS_RSA_WITH_AES_256_CBC_SHA256（Go 未导出常量，手写）
		},
	}

	devLn, err := tls.Listen("tcp", *devAddr, tlsCfg)
	if err != nil {
		log.Fatalf("device listen: %v", err)
	}
	log.Printf("device TLS listening on %s", *devAddr)

	haLn, err := net.Listen("tcp", *haAddr)
	if err != nil {
		log.Fatalf("ha listen: %v", err)
	}
	log.Printf("HA MQTT listening on %s", *haAddr)

	// 假 HTTPS 服务端：接管设备发往阿里云 Link 的 443（激活/OTA/日志上报），
	// 让它以为请求成功，从而不再尝试出网。
	if *httpsAddr != "" {
		startFakeHTTPS(*httpsAddr, cert)
	}

	go func() {
		for {
			c, err := devLn.Accept()
			if err != nil {
				log.Printf("[device] accept: %v", err)
				time.Sleep(200 * time.Millisecond)
				continue
			}
			go b.handleDevice(c)
		}
	}()

	go func() {
		// 心跳：定期刷新每台设备状态，让 HA 看到 online/offline 变化
		for range time.Tick(30 * time.Second) {
			for _, s := range b.sessionList() {
				b.emitState(s)
			}
		}
	}()

	for {
		c, err := haLn.Accept()
		if err != nil {
			log.Printf("[ha] accept: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go b.handleHA(c)
	}
}

// 保证 rsa 包被引用（证书为 RSA 时确保编译进）
var _ = rsa.GenerateKey
