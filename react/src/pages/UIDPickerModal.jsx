import React, { useMemo, useState } from 'react';
import { Empty, Input, Modal, Radio, Space, Spin, Table, Tag, Typography } from 'antd';
import { SearchOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * UIDPickerModal 浮窗式对接码选择器（豪猪 H5 type=8）。
 *
 * 独立 Modal 而不是抽屉内嵌 Select 的原因：
 *   - 每个码的元数据多（价格/库存/运营商/号段类型/省份/更新时间），
 *     Select 下拉行内放不下，换行后列表变得很高很难扫视；
 *   - 需要按价格/库存排序找「便宜且有货」的码，Select 做不了；
 *   - 浮窗用 Table：可排序、可筛选，浏览 50+ 个码的体验接近豪猪后台。
 *
 * 点选行即回填并关闭。置顶码（豪猪运营推荐）排前面并标金色。
 */
export default function UIDPickerModal({ open, onClose, uidItems, loading, onPick, currentUid }) {
  const [keyword, setKeyword] = useState('');
  const [sorter, setSorter] = useState('default'); // default=置顶优先+库存降序

  // 过滤：关键词匹配 uid / 运营商 / 号段类型。
  const filtered = useMemo(() => {
    if (!uidItems) return [];
    const kw = keyword.trim().toLowerCase();
    if (!kw) return uidItems;
    return uidItems.filter(u =>
      u.uid.toLowerCase().includes(kw)
      || (u.isps || []).some(i => (i || '').toLowerCase().includes(kw))
      || (u.segment_type || '').toLowerCase().includes(kw)
      || (u.provinces || []).some(p => (p || '').includes(kw))
    );
  }, [uidItems, keyword]);

  // 排序：default=置顶优先、库存降序；price=价格升序；stock=库存降序。
  const sorted = useMemo(() => {
    const arr = [...filtered];
    if (sorter === 'price') arr.sort((a, b) => a.price - b.price);
    else if (sorter === 'stock') arr.sort((a, b) => b.stock - a.stock);
    else arr.sort((a, b) => (b.pinned - a.pinned) || (b.stock - a.stock));
    return arr;
  }, [filtered, sorter]);

  const columns = [
    {
      title: '',
      key: 'pinned',
      width: 52,
      render: (_, u) => u.pinned ? <Tag color="gold" style={{ marginRight: 0 }}>置顶</Tag> : null,
    },
    {
      title: '对接码',
      dataIndex: 'uid',
      key: 'uid',
      render: (uid, u) => (
        <Space size={6}>
          <Text strong={u.uid === currentUid} copyable={{ text: uid }} style={u.uid === currentUid ? { color: '#1677ff' } : undefined}>
            {uid}
          </Text>
        </Space>
      ),
    },
    {
      title: '价格',
      dataIndex: 'price',
      key: 'price',
      width: 80,
      align: 'right',
      render: p => <Text>{Number(p).toFixed(2)}元</Text>,
    },
    {
      title: '库存',
      dataIndex: 'stock',
      key: 'stock',
      width: 70,
      align: 'right',
      render: s => <Text type={s > 20 ? 'success' : s > 0 ? 'warning' : 'danger'}>{s >= 0 ? s : '未知'}</Text>,
    },
    {
      title: '运营商',
      key: 'isps',
      width: 170,
      render: (_, u) => (
        <Space size={4} wrap>
          {(u.isps || []).slice(0, 5).map(i => <Tag key={i} style={{ marginRight: 0 }}>{i}</Tag>)}
        </Space>
      ),
    },
    {
      title: '号段',
      key: 'segment',
      width: 100,
      render: (_, u) => u.segment_type && u.segment_type !== '未知号段'
        ? <Tag color="orange" style={{ marginRight: 0 }}>{u.segment_type}</Tag>
        : <Text type="secondary">—</Text>,
    },
    {
      title: '最近更新',
      dataIndex: 'updated_at',
      key: 'updated_at',
      width: 150,
      render: t => <Text type="secondary" style={{ fontSize: 12 }}>{t || '—'}</Text>,
    },
  ];

  return (
    <Modal
      title={currentUid ? `选择对接码（当前 ${currentUid}）` : '选择对接码'}
      open={open}
      onCancel={onClose}
      footer={null}
      width={920}
      styles={{ body: { paddingTop: 12 } }}
    >
      <Space style={{ width: '100%', marginBottom: 12 }} direction="vertical" size={8}>
        <Space.Compact style={{ width: '100%' }}>
          <Input
            value={keyword}
            onChange={e => setKeyword(e.target.value)}
            placeholder="过滤：码 / 运营商 / 号段类型 / 省份，如：虚拟、移动"
            prefix={<SearchOutlined />}
            allowClear
            autoFocus
          />
        </Space.Compact>
        <Radio.Group value={sorter} onChange={e => setSorter(e.target.value)} size="small">
          <Radio.Button value="default">置顶优先</Radio.Button>
          <Radio.Button value="price">价格从低到高</Radio.Button>
          <Radio.Button value="stock">库存从高到低</Radio.Button>
        </Radio.Group>
      </Space>
      <div style={{ maxHeight: 460, overflowY: 'auto' }}>
        {loading ? (
          <div style={{ textAlign: 'center', padding: 48 }}><Spin tip="对接码列表加载中…" /></div>
        ) : sorted.length === 0 ? (
          <Empty description={uidItems ? '没有匹配的对接码，换个过滤词' : '暂无对接码数据（先选择项目）'} style={{ padding: 32 }} />
        ) : (
          <Table
            size="small"
            rowKey="uid"
            columns={columns}
            dataSource={sorted}
            pagination={false}
            onRow={u => ({
              onClick: () => { onPick?.(u); onClose?.(); },
              style: { cursor: 'pointer' },
            })}
          />
        )}
      </div>
    </Modal>
  );
}
