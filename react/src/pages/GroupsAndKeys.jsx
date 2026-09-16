import React, { useCallback, useEffect, useState } from 'react';
import {
  Alert, App, Button, Card, Empty, Form, Input, Modal, Popconfirm,
  Space, Table, Tag, Tooltip, Typography,
} from 'antd';
import { DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// GroupsAndKeysPage 分组与密钥管理。
//
// 分组：default 恒存在不可改删（后端兜底），其余可建/改名/删（组内有账号
// 或密钥引用时后端拒绝删除，界面如实报错）。
// 密钥：创建时完整密钥只在弹窗里出现一次；列表脱敏展示。
export default function GroupsAndKeysPage({ api }) {
  const { message } = App.useApp();
  const [groups, setGroups] = useState(null);
  const [keys, setKeys] = useState(null);
  const [loading, setLoading] = useState(false);
  // groupModal: {mode:'create'|'rename', name?}
  const [groupModal, setGroupModal] = useState(null);
  const [keyModal, setKeyModal] = useState(null);
  // createdKey: 新建密钥的一次性完整回显。
  const [createdKey, setCreatedKey] = useState(null);
  const [groupForm] = Form.useForm();
  const [keyForm] = Form.useForm();

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [g, k] = await Promise.all([api('/admin/groups'), api('/admin/keys')]);
      setGroups(g.groups || []);
      setKeys(k.keys || []);
    } catch (err) {
      message.error(err.message);
    } finally {
      setLoading(false);
    }
  }, [api, message]);

  useEffect(() => { load(); }, [load]);

  const groupName = name => (name === 'default' ? <Space><Tag color="blue">default</Tag><Text type="secondary">默认分组</Text></Space> : <Tag>{name}</Tag>);

  const submitGroup = async () => {
    const values = await groupForm.validateFields();
    try {
      if (groupModal.mode === 'create') {
        await api('/admin/groups', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name }) });
        message.success(`分组 ${values.name} 已创建`);
      } else {
        await api(`/admin/groups/${encodeURIComponent(groupModal.name)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name }) });
        message.success('分组已改名（账号归属与密钥绑定已同步迁移）');
      }
      setGroupModal(null);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const deleteGroup = async name => {
    try {
      await api(`/admin/groups/${encodeURIComponent(name)}`, { method: 'DELETE' });
      message.success(`分组 ${name} 已删除`);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const submitKey = async () => {
    const values = await keyForm.validateFields();
    try {
      const created = await api('/admin/keys', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: values.name || '', group: values.group }) });
      setKeyModal(null);
      setCreatedKey(created);
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  const editKey = record => {
    Modal.confirm({
      title: `改绑密钥 ${record.key}`,
      content: (
        <div style={{ marginTop: 12 }}>
          <Paragraph type="secondary">该密钥只能使用所选分组里的账号。</Paragraph>
          <Input defaultValue={record.name} id="key-name-input" placeholder="备注名（可选）" style={{ marginBottom: 8 }} />
          <Input defaultValue={record.group || 'default'} id="key-group-input" placeholder="绑定分组" />
        </div>
      ),
      okText: '保存',
      cancelText: '取消',
      onOk: async () => {
        const name = document.getElementById('key-name-input').value.trim();
        const group = document.getElementById('key-group-input').value.trim() || 'default';
        try {
          await api(`/admin/keys/${encodeURIComponent(record.id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name, group }) });
          message.success('密钥已更新');
          await load();
        } catch (err) {
          message.error(err.message);
          throw err; // 阻止弹窗关闭，让用户看到错误
        }
      },
    });
  };

  const deleteKey = async record => {
    try {
      await api(`/admin/keys/${encodeURIComponent(record.id)}`, { method: 'DELETE' });
      message.success('密钥已删除（立即失效）');
      await load();
    } catch (err) {
      message.error(err.message);
    }
  };

  if (groups === null) {
    return <Card loading />;
  }
  if (!groups.length) {
    return <Empty description="分组存储未启用" />;
  }

  const groupColumns = [
    { title: '分组', dataIndex: 'name', render: groupName },
    { title: '账号数', dataIndex: 'accounts', render: v => <Text>{v}</Text> },
    { title: '绑定密钥', dataIndex: 'keys', render: v => (v ? <Tag color="geekblue">{v} 把</Tag> : <Text type="secondary">无</Text>) },
    {
      title: '操作',
      render: (_, record) => (
        record.default
          ? <Tooltip title="默认分组不可编辑或删除"><Text type="secondary">—</Text></Tooltip>
          : (
            <Space>
              <Button size="small" icon={<EditOutlined />} onClick={() => { setGroupModal({ mode: 'rename', name: record.name }); groupForm.setFieldsValue({ name: record.name }); }}>改名</Button>
              <Popconfirm title={`删除分组 ${record.name}？`} description="组内还有账号（唯一归属）或密钥时会拒绝。" onConfirm={() => deleteGroup(record.name)}>
                <Button size="small" danger icon={<DeleteOutlined />}>删除</Button>
              </Popconfirm>
            </Space>
          )
      ),
    },
  ];

  const keyColumns = [
    { title: '密钥', dataIndex: 'key', render: v => <Text code copyable={{ text: v }}>{v}</Text> },
    { title: '备注', dataIndex: 'name', render: v => v || <Text type="secondary">-</Text> },
    { title: '分组', dataIndex: 'group', render: v => <Tag color={v === 'default' ? 'blue' : 'purple'}>{v || '不限'}</Tag> },
    { title: '创建时间', dataIndex: 'created_at', render: v => (v ? new Date(v).toLocaleString() : '-') },
    {
      title: '操作',
      render: (_, record) => (
        <Space>
          <Button size="small" icon={<EditOutlined />} onClick={() => editKey(record)}>改绑</Button>
          <Popconfirm title="删除这把密钥？" description="删除后立即失效，用它的调用会全部 401。" onConfirm={() => deleteKey(record)}>
            <Button size="small" danger icon={<DeleteOutlined />}>删除</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Card
        title="账号分组"
        extra={(
          <Space>
            <Button icon={<ReloadOutlined />} loading={loading} onClick={load}>刷新</Button>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => { setGroupModal({ mode: 'create' }); groupForm.resetFields(); }}>新建分组</Button>
          </Space>
        )}
      >
        <Paragraph type="secondary">账号可属于多个分组（在账号列表里设置）；密钥绑定一个分组后只能使用该分组的账号。</Paragraph>
        <Table rowKey="name" size="small" pagination={false} columns={groupColumns} dataSource={groups} />
      </Card>

      <Card
        title="API 密钥"
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={() => { setKeyModal(true); keyForm.resetFields(); keyForm.setFieldsValue({ group: 'default' }); }}>新建密钥</Button>}
      >
        <Paragraph type="secondary">分组密钥只能使用绑定分组的账号；config 里的管理员密钥（api_key）不受分组限制，也不会出现在此列表。</Paragraph>
        {keys.length
          ? <Table rowKey="id" size="small" pagination={false} columns={keyColumns} dataSource={keys} />
          : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有创建密钥" />}
      </Card>

      <Modal
        open={!!groupModal}
        title={groupModal?.mode === 'create' ? '新建分组' : `改名分组 ${groupModal?.name}`}
        onOk={submitGroup}
        onCancel={() => setGroupModal(null)}
        okText={groupModal?.mode === 'create' ? '创建' : '改名'}
        cancelText="取消"
      >
        <Form form={groupForm} layout="vertical">
          <Form.Item
            name="name"
            label="分组名"
            rules={[
              { required: true, message: '请输入分组名' },
              { pattern: /^[A-Za-z0-9\-_.]{1,32}$/, message: '字母、数字与 - _ .，最多 32 字符' },
            ]}
          >
            <Input placeholder="例如 vip、cn-pool" maxLength={32} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={!!keyModal}
        title="新建 API 密钥"
        onOk={submitKey}
        onCancel={() => setKeyModal(null)}
        okText="创建"
        cancelText="取消"
      >
        <Form form={keyForm} layout="vertical">
          <Form.Item name="name" label="备注名（可选）">
            <Input placeholder="例如：给 NewAPI 的中转密钥" maxLength={64} />
          </Form.Item>
          <Form.Item name="group" label="绑定分组" rules={[{ required: true, message: '请选择分组' }]}>
            <Input placeholder="default" />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={!!createdKey}
        title="密钥已创建"
        onOk={() => setCreatedKey(null)}
        onCancel={() => setCreatedKey(null)}
        okText="我已保存"
        cancelButtonProps={{ style: { display: 'none' } }}
      >
        <Alert
          type="warning"
          showIcon
          message="完整密钥仅此一次展示"
          description="关闭后无法再次查看，请立即复制保存到你的调用方配置里。"
        />
        <Paragraph style={{ marginTop: 12 }}>
          <Text code copyable={{ text: createdKey?.key }}>{createdKey?.key}</Text>
        </Paragraph>
        <Space>
          <Tag color={createdKey?.group === 'default' ? 'blue' : 'purple'}>{createdKey?.group}</Tag>
          {createdKey?.name && <Text type="secondary">{createdKey.name}</Text>}
        </Space>
      </Modal>
    </Space>
  );
}
