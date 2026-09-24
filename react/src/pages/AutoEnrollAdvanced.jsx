import React, { useEffect, useState } from 'react';
import {
  Alert, Button, Divider, Drawer, Form, Input, InputNumber, message, Modal, Popconfirm, Select, Typography,
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
 * 两组参数（P0）：
 *   - 取号策略：对接码 uid / 运营商优先级 isp / 对接方标识 author
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

  // 每次打开抽屉都用最新生效值重置表单：不残留上次草稿。
  useEffect(() => {
    if (open) {
      form.setFieldsValue({
        uid: hz.uid || '',
        isp: (hz.isp || '').split(',').map(s => s.trim()).filter(Boolean),
        author: hz.author || '',
        min_balance: Number(ae.min_balance ?? DEFAULTS.min_balance),
        consecutive_fails: Number(ae.consecutive_fails ?? DEFAULTS.consecutive_fails),
        retry_delay_seconds: Number(ae.retry_delay_seconds ?? DEFAULTS.retry_delay_seconds),
      });
      setDirty(false);
      setSaveError('');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

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
      width={560}
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
                uid: '', isp: [], author: '',
                min_balance: DEFAULTS.min_balance,
                consecutive_fails: DEFAULTS.consecutive_fails,
                retry_delay_seconds: DEFAULTS.retry_delay_seconds,
              });
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

      <Form
        form={form}
        layout="horizontal"
        labelCol={{ span: 8 }}
        wrapperCol={{ span: 16 }}
        onValuesChange={() => setDirty(true)}
      >
        <Divider orientation="left" plain style={{ margin: '4px 0 12px' }}>取号策略</Divider>
        <Form.Item
          name="uid"
          label="对接码"
          extra="留空 = 平台随机分配。指定有号的对接码成功率更高（实测 6/6 vs 随机 3/6）。在豪猪后台「项目详情」查看可用对接码。"
          validateTrigger={false}
          rules={[{
            validator: (_, v) => (!v || /^\d+-[A-Za-z0-9]+$/.test(v.trim()))
              ? Promise.resolve()
              : Promise.reject(new Error('格式形如 52283-WW9L2J4WOL')),
          }]}
        >
          <Input allowClear placeholder="如 52283-WW9L2J4WOL，留空自动分配" />
        </Form.Item>
        <Form.Item
          name="isp"
          label="运营商优先级"
          extra="依次降级尝试，最后退回不限；都不选 = 直接不限。选对接码时可参考豪猪后台标注的运营商。"
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
