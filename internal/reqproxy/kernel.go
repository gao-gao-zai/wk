// Package kernel 将 NodeSpec 转成 Xray outbound 并管理内嵌 Xray 实例。
//
// 槽位机制（设计文档 3.3/3.3b）：
//   - 每个槽位一个本地 inbound（tag = slot_id，独立端口，http 代理类型）；
//   - 每个槽位一个 outbound（tag = slot_id，定义 = 当前指向节点的拷贝）；
//   - 路由规则 inboundTag=slot_id → outboundTag=slot_id，永久静态；
//   - 换节点只换 slot_id 的 outbound（RemoveHandler + AddOutboundHandler）。
//
// 实现路径（经 Xray-core 调研确认）：
//   JSON 片段 → conf.InboundDetourConfig/OutboundDetourConfig.Build()
//   → core.AddInboundHandler/AddOutboundHandler；删除经
//   inbound/outbound Manager.RemoveHandler；路由经 routing.Router.AddRule。
package reqproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/blackhole"

	_ "github.com/xtls/xray-core/main/distro/all" // 注册全部协议/feature（init 副作用）
)

// Kernel 内嵌 Xray 实例管理器。
type Kernel struct {
	mu        sync.Mutex
	inst      *core.Instance
	ctx       context.Context
	outbounds map[string]bool // 已存在的出站 tag
	rules     map[string]bool // 已存在的路由 ruleTag（= slot_id）
}

// NewKernel 启动最小 Xray 实例（dispatcher + proxyman + 默认 blackhole 出口）。
func NewKernel() (*Kernel, error) {
	cfg := &core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&dispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&router.Config{}),
		},
		Outbound: []*core.OutboundHandlerConfig{
			// 占位默认出口：blackhole（未被路由规则匹配的流量丢弃——本模块所有
			// inbound 都有钉死规则，理论上到不了这里；防御性兜底）
			{
				Tag:            "blackhole-default",
				SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{}),
				ProxySettings:  serial.ToTypedMessage(&blackhole.Config{}),
			},
		},
	}
	inst, err := core.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 Xray 实例: %w", err)
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		return nil, fmt.Errorf("启动 Xray 实例: %w", err)
	}
	return &Kernel{
		inst:      inst,
		ctx:       context.Background(),
		outbounds: map[string]bool{},
		rules:     map[string]bool{},
	}, nil
}

// Close 关闭实例。
func (k *Kernel) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inst != nil {
		err := k.inst.Close()
		k.inst = nil
		return err
	}
	return nil
}

// ---- 槽位操作 ----

// HasSlot 内核里是否已有该槽位（幂等恢复用）。
func (k *Kernel) HasSlot(slotID string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.rules[slotID]
}

// AddSlot 开槽位：inbound（固定端口）+ 路由规则 + 指向 node 的 outbound。
// inbound 和路由只在此时创建，之后永不重建（设计文档 3.3 热更新约定）。
func (k *Kernel) AddSlot(slotID string, port int, node NodeSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inst == nil {
		return fmt.Errorf("内核已关闭")
	}
	// inbound JSON（标准 xray 配置片段格式）
	inJSON := fmt.Sprintf(`{
		"tag": %q, "listen": "127.0.0.1", "port": %d,
		"protocol": "http", "settings": {}
	}`, slotID, port)
	var ic conf.InboundDetourConfig
	if err := json.Unmarshal([]byte(inJSON), &ic); err != nil {
		return fmt.Errorf("inbound 配置构造: %w", err)
	}
	ih, err := ic.Build()
	if err != nil {
		return fmt.Errorf("inbound Build: %w", err)
	}
	if err := core.AddInboundHandler(k.inst, ih); err != nil {
		return fmt.Errorf("AddInbound(%s):%d: %w", slotID, port, err)
	}

	// 路由规则：inboundTag=slot_id → outboundTag=slot_id
	ruleJSON := fmt.Sprintf(`{"ruleTag": %q, "inboundTag": [%q], "outboundTag": %q}`, slotID, slotID, slotID)
	var rc conf.RouterConfig
	rc.RuleList = []json.RawMessage{json.RawMessage(ruleJSON)}
	routedCfg, err := rc.Build()
	if err != nil {
		return fmt.Errorf("路由规则 Build: %w", err)
	}
	rm, ok := k.inst.GetFeature(routing.RouterType()).(routing.Router)
	if !ok {
		return fmt.Errorf("Xray 路由模块不可用")
	}
	if err := rm.AddRule(serial.ToTypedMessage(routedCfg), true); err != nil {
		return fmt.Errorf("AddRule(%s): %w", slotID, err)
	}
	k.rules[slotID] = true

	// 槽位 outbound（定义 = 节点拷贝）
	return k.addOutboundLocked(slotID, node)
}

// RetargetSlot 换槽位指向：只替换 slot_id 的 outbound。inbound/路由不动。
func (k *Kernel) RetargetSlot(slotID string, node NodeSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inst == nil {
		return fmt.Errorf("内核已关闭")
	}
	k.removeOutboundLocked(slotID)
	return k.addOutboundLocked(slotID, node)
}

// RemoveSlot 删槽位（含 inbound/路由/outbound）。端口随之释放。
func (k *Kernel) RemoveSlot(slotID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inst == nil {
		return fmt.Errorf("内核已关闭")
	}
	// outbound
	k.removeOutboundLocked(slotID)
	// 路由
	if k.rules[slotID] {
		if rm, ok := k.inst.GetFeature(routing.RouterType()).(routing.Router); ok {
			_ = rm.RemoveRule(slotID)
		}
		delete(k.rules, slotID)
	}
	// inbound
	if im, ok := k.inst.GetFeature(inbound.ManagerType()).(inbound.Manager); ok {
		_ = im.RemoveHandler(k.ctx, slotID)
	}
	return nil
}

// ---- 节点 outbound（非槽位） ----

// AddNodeOutbound 添加节点出站（tag = node_id；测速临时通道用）。
func (k *Kernel) AddNodeOutbound(node NodeSpec) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.inst == nil {
		return fmt.Errorf("内核已关闭")
	}
	return k.addOutboundLocked(node.ID, node)
}

// RemoveNodeOutbound 删除节点出站。
func (k *Kernel) RemoveNodeOutbound(tag string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.removeOutboundLocked(tag)
}

// ---- 内部（须持锁） ----

func (k *Kernel) addOutboundLocked(tag string, node NodeSpec) error {
	ocJSON, err := buildOutboundJSON(tag, node)
	if err != nil {
		return err
	}
	var oc conf.OutboundDetourConfig
	if err := json.Unmarshal(ocJSON, &oc); err != nil {
		return fmt.Errorf("outbound 配置构造(%s): %w", tag, err)
	}
	oh, err := oc.Build()
	if err != nil {
		return fmt.Errorf("outbound Build(%s): %w", tag, err)
	}
	if err := core.AddOutboundHandler(k.inst, oh); err != nil {
		return fmt.Errorf("AddOutbound(%s): %w", tag, err)
	}
	k.outbounds[tag] = true
	return nil
}

func (k *Kernel) removeOutboundLocked(tag string) error {
	if !k.outbounds[tag] {
		return nil
	}
	if om, ok := k.inst.GetFeature(outbound.ManagerType()).(outbound.Manager); ok {
		if err := om.RemoveHandler(k.ctx, tag); err != nil {
			return fmt.Errorf("RemoveOutbound(%s): %w", tag, err)
		}
	}
	delete(k.outbounds, tag)
	return nil
}

// blackholeConfig 已由 blackhole.Config 类型直接提供（import proxy/blackhole）。

// buildOutboundJSON 把 NodeSpec 转成 xray outbound JSON 片段（bytes）。
// 协议字段映射：NodeSpec.Spec（parse 层归一化产物）→ xray 标准字段名。
func buildOutboundJSON(tag string, n NodeSpec) ([]byte, error) {
	m := map[string]any{"tag": tag, "protocol": n.Protocol}
	settings, stream, err := outboundParts(n)
	if err != nil {
		return nil, err
	}
	if settings != nil {
		m["settings"] = settings
	}
	if stream != nil {
		m["streamSettings"] = stream
	}
	return json.Marshal(m)
}

// outboundParts 生成 settings 和 streamSettings 两个 JSON 片段。
func outboundParts(n NodeSpec) (settings, stream map[string]any, err error) {
	spec := n.Spec
	switch n.Protocol {
	case "vmess":
		vnext := map[string]any{
			"address": spec["host"], "port": spec["port"],
			"users": []map[string]any{{
				"id": spec["uuid"], "security": orDefault(spec["security"], "auto"),
				"alterId": orDefault(spec["alterId"], 0),
			}},
		}
		// VMessOutboundConfig 顶层扁平：Address/Port/ID 自动包装
		settings = map[string]any{
			"address": spec["host"], "port": spec["port"],
			"id": spec["uuid"], "security": orDefault(spec["security"], "auto"),
			"vnext": []map[string]any{vnext},
		}
	case "vless":
		user := map[string]any{"id": spec["uuid"], "encryption": "none"}
		if f, ok := spec["flow"]; ok && f != "" {
			user["flow"] = f
		}
		settings = map[string]any{
			"vnext": []map[string]any{{
				"address": spec["host"], "port": spec["port"],
				"users": []map[string]any{user},
			}},
		}
	case "trojan":
		settings = map[string]any{
			"servers": []map[string]any{{
				"address": spec["host"], "port": spec["port"],
				"password": spec["password"],
			}},
		}
	case "ss":
		settings = map[string]any{
			"servers": []map[string]any{{
				"address": spec["host"], "port": spec["port"],
				"method":  spec["method"], "password": spec["password"],
			}},
		}
	case "socks", "http":
		// Xray 的 HTTPClientConfig/SocksClientConfig 嵌套形态只认
		// servers[].users 数组（每个 user: {user, pass}）。把 user/pass
		// 平铺在 server 对象里会被静默忽略 → outbound 无凭据 → 407。
		server := map[string]any{"address": spec["host"], "port": spec["port"]}
		if u, ok := spec["username"]; ok && u != "" {
			server["users"] = []map[string]any{{"user": u, "pass": orDefault(spec["password"], "")}}
		}
		settings = map[string]any{"servers": []map[string]any{server}}
	case "wireguard":
		// 简化：xray wireguard outbound JSON
		peers := []map[string]any{{
			"publicKey": orDefault(spec["peerPublicKey"], ""),
			"endpoint":  fmt.Sprintf("%v:%v", spec["host"], spec["port"]),
		}}
		settings = map[string]any{
			"secretKey":  orDefault(spec["privateKey"], ""),
			"address":   orDefault(spec["localAddress"], []string{"172.16.0.2/32"}),
			"peers":     peers,
			"reserved":  []int{1, 2, 3},
		}
	default:
		return nil, nil, fmt.Errorf("未知协议 %q", n.Protocol)
	}

	// 传输层
	stream, err = streamSettings(spec)
	if err != nil {
		return nil, nil, err
	}
	return settings, stream, nil
}

// streamSettings 从归一化 spec 生成 xray streamSettings JSON 片段。
func streamSettings(spec map[string]any) (map[string]any, error) {
	network, _ := spec["network"].(string)
	if network == "" {
		network = "tcp"
	}
	// xray 新版网络名："tcp"→"raw"；ws/grpc/httpupgrade/xhttp 保留
	if network == "tcp" {
		network = "raw"
	}
	out := map[string]any{"network": network}

	sec, _ := spec["security"].(string)
	tlsOn, _ := spec["tls"].(bool)
	switch {
	case sec == "reality":
		out["security"] = "reality"
		rs := map[string]any{
			"fingerprint": orDefault(spec["fingerprint"], "chrome"),
			"serverName":  orDefault(spec["serverName"], spec["host"]),
			"publicKey":   orDefault(spec["publicKey"], ""),
		}
		if sid, ok := spec["shortId"]; ok && sid != "" {
			rs["shortId"] = sid
		}
		out["realitySettings"] = rs
	case tlsOn:
		out["security"] = "tls"
		ts := map[string]any{"serverName": orDefault(spec["serverName"], spec["host"])}
		if alpn, ok := spec["alpn"]; ok {
			ts["alpn"] = alpn
		}
		if fp, ok := spec["fingerprint"]; ok && fp != "" {
			ts["fingerprint"] = fp
		}
		out["tlsSettings"] = ts
	}

	switch network {
	case "ws":
		ws := map[string]any{"path": orDefault(spec["path"], "/")}
		if h, ok := spec["wsHost"]; ok && h != "" {
			ws["host"] = h
		}
		out["wsSettings"] = ws
	case "grpc":
		g := map[string]any{"serviceName": orDefault(spec["serviceName"], "")}
		if h, ok := spec["wsHost"]; ok && h != "" {
			g["authority"] = h
		}
		out["grpcSettings"] = g
	case "httpupgrade":
		hu := map[string]any{"path": orDefault(spec["path"], "/")}
		if h, ok := spec["wsHost"]; ok && h != "" {
			hu["host"] = h
		}
		out["httpupgradeSettings"] = hu
	case "xhttp", "splithttp":
		xh := map[string]any{"path": orDefault(spec["path"], "/")}
		if h, ok := spec["wsHost"]; ok && h != "" {
			xh["host"] = h
		}
		out["xhttpSettings"] = xh
	}
	return out, nil
}

func orDefault(v any, def any) any {
	if v == nil || v == "" {
		return def
	}
	return v
}

// dedupe：抑制未用 import 警告（bytes/strings 暂未直接使用则删）
var _ = bytes.MinRead
var _ = strings.TrimSpace
