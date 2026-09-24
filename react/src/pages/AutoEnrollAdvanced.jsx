import React, { useEffect, useRef, useState } from 'react';
import {
  Alert, AutoComplete, Button, Divider, Drawer, Form, Input, InputNumber, message, Modal, Popconfirm, Select, Space, Tag, Typography,
} from 'antd';

const { Text, Paragraph } = Typography;

// 高级设置抽屉的默认值（与后端 defaultMinBalance/defaultConsecutiveFails 一致）。
const DEFAULTS = { min_balance: 2.2, consecutive_fails: 15, retry_delay_seconds: 5 };

/**
 * AutoEnrollAdvanced 自动加号「高级设置」抽屉。
 *
 * 与主页面的"逐项即时保存"相反，这里是**低频批量改统一提交**模式：
 * 所有修改以底部「保存高级设置」为唯一提交点，有脏改动关抽屉先确认。
 *
 * 三组参数：
 *   - 项目与对接码（P1 增强）：项目搜索选择（H5 会话可用时）/ 对接码
 *     列表选择（带价格/库存/运营商标签）；H5 未配置或失效时自动退化
 *     为手填输入框，功能不缺失只是少辅助。
 *   - 取号策略：运营商优先级 isp / 对接方标识 author
 *   - 任务控制：余额保护阈值 / 连续失败熔断 / 尝试间隔
 *
 * 保存时 diff：haozhuma 字段与 autoenroll 字段分两个 patch 提交
 * （两次都成功才提示保存成功；失败保留抽屉打开 + 错误常驻顶部）。
 */
export default function AutoEnrollAdvanced({ open, onClose, api, config, running, onSaveHaozhuma, onSaveAutoEnroll }) {
  const [form] = Form.useForm();
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');

  const hz = config.sms?.haozhuma || {};
  const ae = config.autoenroll || {};
  const h5 = hz.h5 || {};
  const h5Ready = h5.has === true && h5.expired !== true;

  // —— 项目搜索（H5 type=30 代理） ——
  const [projectOptions, setProjectOptions] = useState([]);
  const [searching, setSearching] = useState(false);
  const [searched, setSearched] = useState(false); // 是否已搜过（空态文案区分）
  const [searchError, setSearchError] = useState('');
  const searchTimer = useRef(null);

  // —— 对接码列表（H5 type=8 代理，按选中的项目 sid 拉取） ——
  // 注意：type=8 只认 16 位 hex 项目标识（type=30 返回的 sid），不认
  // 数字项目 ID（实测 61904 → "没有数据"，hex → 正常返回）。表单里存
  // 的是数字 ID（写入配置用），映射关系记在 hexSidMap。
  const [hexSidMap, setHexSidMap] = useState({}); // 数字ID → hex sid
  const [uidItems, setUidItems] = useState(null); // null=未加载（手填态），[]=已加载无数据
  const [uidLoading, setUidLoading] = useState(false);
  const [uidError, setUidError] = useState('');

  // 选中项目后加载对接码列表。参数是数字项目 ID（表单值）。
  const loadUIDs = async (projectId) => {
    if (!h5Ready || !projectId) return;
    const hexSid = hexSidMap[projectId];
    if (!hexSid) {
      // 没有映射（手填的项目 ID / 映射丢失）：先搜索该 ID 拿 hex sid。
      try {
        const resp = await api(`/admin/account/sms/haozhuma/projects?q=${encodeURIComponent(projectId)}`);
        const hit = (resp.projects || []).find(p => p.project_id === projectId);
        if (!hit) { setUidItems(null); return; }
        setHexSidMap(current => ({ ...current, [projectId]: hit.sid }));
        loadUIDsByHex(hit.sid);
      } catch {
        setUidItems(null); // 搜不到就保持手填
      }
      return;
    }
    loadUIDsByHex(hexSid);
  };

  const loadUIDsByHex = async (hexSid) => {
    setUidLoading(true);
    setUidError('');
    try {
      const resp = await api(`/admin/account/sms/haozhuma/uids?sid=${encodeURIComponent(hexSid)}`);
      setUidItems(resp.uids || []);
    } catch (err) {
      setUidError(err.message);
      setUidItems(null); // 失败退化为手填
    } finally {
      setUidLoading(false);
    }
  };

  // 每次打开抽屉都用最新生效值重置表单：不残留上次草稿。
  useEffect(() => {
    if (open) {
      form.setFieldsValue({
        sid: hz.sid || '',
        uid: hz.uid || '',
        isp: (hz.isp || '').split(',').map(s => s.trim()).filter(Boolean),
        author: hz.author || '',
        min_balance: Number(ae.min_balance ?? DEFAULTS.min_balance),
        consecutive_fails: Number(ae.consecutive_fails ?? DEFAULTS.consecutive_fails),
        retry_delay_seconds: Number(ae.retry_delay_seconds ?? DEFAULTS.retry_delay_seconds),
      });
      setDirty(false);
      setSaveError('');
      setProjectOptions([]);
      setSearched(false);
      setSearchError('');
      setHexSidMap({});
      setUidItems(null);
      setUidError('');
      // 已有 sid 且 H5 可用：自动拉一次对接码列表（内部会先搜索补映射）。
      if (h5Ready && hz.sid) loadUIDs(hz.sid);
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }
  }, [open]);

  const searchProjects = async (keyword) => {
    const q = (keyword || '').trim();
    if (!q) return;
    setSearching(true);
    setSearchError('');
    try {
      const resp = await api(`/admin/account/sms/haozhuma/projects?q=${encodeURIComponent(q)}`);
      const projects = resp.projects || [];
      setProjectOptions(projects);
      // 记录 数字ID → hex sid 映射（对接码列表只认 hex，实测数字 ID
      // 返回"没有数据"）。
      setHexSidMap(current => {
        const next = { ...current };
        projects.forEach(p => { if (p.project_id) next[p.project_id] = p.sid; });
        return next;
      });
      setSearched(true);
    } catch (err) {
      setSearchError(err.message);
      setProjectOptions([]);
      setSearched(false);
    } finally {
      setSearching(false);
    }
  };

  const handleClose = () => {
    if (dirty) {
      Modal.confirm({
        title: '有未保存的修改',
        content: '关闭后将丢弃这些改动。',
        okText: '丢弃并关闭',
        okButtonProps: { danger: true },
        cancelText: '继续编辑',
        onOk: onClose,
      });
      return;
    }
    onClose();
  };

  // 保存：diff 出显式改动的字段分两组提交。未变的字段不发（指针语义，
  // 后端增量合并）。
  const save = async () => {
    const values = await form.validateFields();
    setSaving(true);
    setSaveError('');
    try {
      const haozhumaPatch = {};
      if ((values.sid || '') !== (hz.sid || '')) haozhumaPatch.sid = values.sid || '';
      if ((values.uid || '') !== (hz.uid || '')) haozhumaPatch.uid = values.uid || '';
      const ispJoined = (values.isp || []).join(',');
      if (ispJoined !== (hz.isp || '')) haozhumaPatch.isp = ispJoined;
      if ((values.author || '') !== (hz.author || '')) haozhumaPatch.author = values.author || '';

      const aePatch = {};
      const mb = Number(values.min_balance);
      if (mb !== Number(ae.min_balance ?? DEFAULTS.min_balance)) aePatch.min_balance = mb;
      const cf = Number(values.consecutive_fails);
      if (cf !== Number(ae.consecutive_fails ?? DEFAULTS.consecutive_fails)) aePatch.consecutive_fails = cf;
      const rd = Number(values.retry_delay_seconds);
      if (rd !== Number(ae.retry_delay_seconds ?? DEFAULTS.retry_delay_seconds)) aePatch.retry_delay_seconds = rd;

      if (!Object.keys(haozhumaPatch).length && !Object.keys(aePatch).length) {
        setSaveError('没有改动需要保存');
        setSaving(false);
        return;
      }
      const results = [];
      if (Object.keys(haozhumaPatch).length) results.push(await onSaveHaozhuma?.(haozhumaPatch));
      if (Object.keys(aePatch).length) results.push(await onSaveAutoEnroll?.(aePatch));
      if (results.some(r => !r)) {
        // 具体 message.error 已由 saveHaozhuma/saveAutoEnroll 弹过；这里只
        // 保持抽屉打开，让用户修正后重试。
        setSaveError('部分设置保存失败，请检查后重试（未成功的项未生效）');
        setSaving(false);
        return;
      }
      message.success('高级设置已保存并即时生效');
      setDirty(false);
      onClose();
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      title="高级设置"
      width={600}
      open={open}
      onClose={handleClose}
      destroyOnClose
      footer={(
        <div style={{ display: 'flex', justifyContent: 'space-between' }}>
          <Popconfirm
            title="恢复默认值？"
            description="只重置表单（不保存），仍需点「保存高级设置」提交。"
            onConfirm={() => {
              form.setFieldsValue({
                sid: '', uid: '', isp: [], author: '',
                min_balance: DEFAULTS.min_balance,
                consecutive_fails: DEFAULTS.consecutive_fails,
                retry_delay_seconds: DEFAULTS.retry_delay_seconds,
              });
              setUidItems(null);
              setDirty(true);
            }}
          >
            <Button type="text">恢复默认</Button>
          </Popconfirm>
          <div>
            <Button onClick={handleClose} style={{ marginRight: 8 }}>取消</Button>
            <Button type="primary" loading={saving} onClick={save} disabled={!dirty}>
              保存高级设置
            </Button>
          </div>
        </div>
      )}
    >
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        不常用参数；保存即写入 config.json 并即时生效（新取号立刻用新值，在途号码按旧值收尾）。
      </Paragraph>
      {saveError && <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={saveError} />}
      {!h5Ready && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={h5.expired ? 'H5 会话已失效，选择器退化为手填' : '未配置 H5 会话，项目/对接码为手填模式'}
          description={
            h5.expired
              ? '到「自动加号 → 鉴权与增强设置」重新粘贴 PHPSESSID 后，这里会升级为搜索选择。'
              : '在「自动加号 → 鉴权与增强设置」粘贴 PHPSESSID 后，这里会升级为带价格/库存的项目搜索与对接码选择。'
          }
        />
      )}

      <Form
        form={form}
        layout="horizontal"
        labelCol={{ span: 8 }}
        wrapperCol={{ span: 16 }}
        onValuesChange={(changed) => {
          setDirty(true);
          // 项目切换后重新拉对接码列表（H5 可用时）。
          if (changed.sid !== undefined && h5Ready) {
            setUidItems(null);
            setUidError('');
            if (changed.sid) loadUIDs(changed.sid);
          }
        }}
      >
        <Divider orientation="left" plain style={{ margin: '4px 0 12px' }}>项目与对接码</Divider>
        <Form.Item
          name="sid"
          label="项目 ID"
          extra={h5Ready ? '输入关键词（如"腾讯"）搜索，点选后自动写入项目 ID。' : '取号项目 ID，如 52283（腾讯科技[限对接]）。'}
          validateTrigger={false}
          rules={[{
            validator: (_, v) => (!v || /^\d+$/.test(v.trim()))
              ? Promise.resolve()
              : Promise.reject(new Error('项目 ID 是纯数字，如 52283')),
          }]}
        >
          {h5Ready ? (
            <AutoComplete
              options={projectOptions.map(p => ({
                value: p.project_id || p.sid,
                label: (
                  <div>
                    <Text strong>{p.project_id ? `【${p.project_id}】` : ''}{(p.name || '').replace(/^【\d+】/, '').replace(/\s*\[[0-9a-f]+\]$/, '')}</Text>
                    <Text type="secondary" style={{ marginLeft: 8, fontSize: 12 }}>{p.project_id ? '' : '（未能解析项目ID）'}</Text>
                  </div>
                ),
              }))}
              onSearch={(kw) => {
                // 300ms 防抖：输入停顿后再搜（每次击键都打上游没必要）。
                if (searchTimer.current) clearTimeout(searchTimer.current);
                if (!kw.trim()) { setProjectOptions([]); setSearched(false); return; }
                searchTimer.current = setTimeout(() => searchProjects(kw), 300);
              }}
              notFoundContent={
                searching ? '搜索中…'
                  : searchError ? <Text type="danger">{searchError}</Text>
                    : searched ? '没有匹配的项目，换个关键词（如：腾讯、微信、抖音）'
                      : '输入关键词搜索，如"腾讯"'
              }
              placeholder="输入关键词搜索，如：腾讯"
              allowClear
            />
          ) : (
            <Input allowClear placeholder="如 52283" />
          )}
        </Form.Item>
        <Form.Item
          name="uid"
          label="对接码"
          extra={
            uidError ? <Text type="danger">{uidError}（已退化为手填）</Text>
              : uidItems != null ? '带价格/库存/运营商标签，置顶款标星。选中的码会钉死专属通道（成功率更高）。'
                : '留空 = 平台随机分配。指定有号的对接码成功率更高（实测 6/6 vs 随机 3/6）。'
          }
          validateTrigger={false}
          rules={[{
            validator: (_, v) => (!v || /^\d+-[A-Za-z0-9]+$/.test(v.trim()))
              ? Promise.resolve()
              : Promise.reject(new Error('格式形如 52283-WW9L2J4WOL')),
          }]}
        >
          {h5Ready && uidItems != null && uidItems.length > 0 ? (
            <Select
              showSearch
              allowClear
              placeholder="选择对接码"
              optionFilterProp="value"
              loading={uidLoading}
              options={uidItems.map(u => ({
                value: u.uid,
                label: (
                  <div>
                    <Space size={4} wrap>
                      {u.pinned && <Tag color="gold" style={{ marginRight: 0 }}>置顶</Tag>}
                      <Text strong>{u.uid}</Text>
                      <Text type="secondary">{Number(u.price).toFixed(2)}元</Text>
                      <Tag style={{ marginRight: 0 }}>{u.stock >= 0 ? `库存 ${u.stock}` : '库存未知'}</Tag>
                      {(u.isps || []).length > 0 && <Tag style={{ marginRight: 0 }}>{u.isps.join('/')}</Tag>}
                      {u.segment_type && u.segment_type !== '未知号段' && (
                        <Tag color="orange" style={{ marginRight: 0 }}>{u.segment_type}</Tag>
                      )}
                    </Space>
                  </div>
                ),
              }))}
            />
          ) : h5Ready && uidLoading ? (
            <Select loading placeholder="对接码列表加载中…" />
          ) : (
            <Input allowClear placeholder="如 52283-WW9L2J4WOL，留空自动分配" />
          )}
        </Form.Item>
        <Form.Item
          name="isp"
          label="运营商优先级"
          extra={uidItems != null && uidItems.length > 0
            ? '依次降级尝试，最后退回不限；都不选 = 直接不限。可参考所选对接码标注的运营商。'
            : '依次降级尝试，最后退回不限；都不选 = 直接不限。选对接码时可参考豪猪后台标注的运营商。'}
        >
          <Select
            mode="multiple"
            allowClear
            placeholder="不限"
            options={[
              { value: '1', label: '移动' },
              { value: '2', label: '联通' },
              { value: '3', label: '电信' },
            ]}
          />
        </Form.Item>
        <Form.Item
          name="author"
          label="对接方标识"
          extra="「[限对接]」项目的对接方标识。52283 项目实测留空才能取到号，除非明确知道该填什么，否则保持留空。"
        >
          <Input allowClear placeholder="留空" />
        </Form.Item>

        <Divider orientation="left" plain style={{ margin: '20px 0 12px' }}>任务控制</Divider>
        <Form.Item
          name="min_balance"
          label="余额保护阈值"
          extra="豪猪余额低于此值不再取新号（只在收码成功时扣费，留够一次的钱即可）。0 = 关闭保护——可能收到码却扣不了费。"
        >
          <InputNumber min={0} max={1000} step={0.1} addonAfter="元" style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item
          name="consecutive_fails"
          label="连续失败熔断"
          extra="连续这么多个号都失败说明通道坏了，及时止损停止任务。失败号不扣费。"
        >
          <InputNumber min={1} max={100} addonAfter="次" style={{ width: '100%' }} />
        </Form.Item>
        <Form.Item
          name="retry_delay_seconds"
          label="尝试间隔"
          extra="两次取号尝试之间的间隔，过密会把代理池/上游打得太紧。"
        >
          <InputNumber min={1} max={60} addonAfter="秒" style={{ width: '100%' }} />
        </Form.Item>
      </Form>
    </Drawer>
  );
}
