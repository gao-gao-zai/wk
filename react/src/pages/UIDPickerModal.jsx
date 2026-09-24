import React, { useEffect, useMemo, useState } from 'react';
import { Button, Empty, Input, message, Modal, Radio, Space, Spin, Table, Tag, Typography } from 'antd';
import { PlusOutlined, SearchOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * UIDPickerModal 浮窗式对接码选择器（豪猪 H5 type=8 + type=3/type=4）。
 *
 * 独立 Modal 而不是抽屉内嵌 Select 的原因：
 *   - 每个码的元数据多（价格/库存/运营商/号段类型/省份/更新时间），
 *     Select 下拉行内放不下，换行后列表变得很高很难扫视；
 *   - 需要按价格/库存排序找「便宜且有货」的码，Select 做不了；
 *   - 浮窗用 Table：可排序、可筛选，浏览 50+ 个码的体验接近豪猪后台。
 *
 * 「加入」语义（关键业务逻辑）：官方取号 API 只认**已加入账户**的对接码
 * （市场列表看得见 ≠ 拥有；未加入直接取号报「没有这个[xxx]专属码」）。
 * 所以这里做两步：打开时拉「我的对接码」（type=3）标绿已加入的行；
 * 点未加入的行 = 先调后端 add-uid（type=4）加入，成功才回填选中。
 * 加入失败（会话失效等）弹错误，不回填。
 */
export default function UIDPickerModal({ open, onClose, uidItems, loading, onPick, currentUid, api }) {
  const [keyword, setKeyword] = useState('');
  const [sorter, setSorter] = useState('default'); // default=置顶优先+库存降序
  const [mySet, setMySet] = useState(null); // Set<string> 已加入的码；null=未加载
  const [addingUid, setAddingUid] = useState(''); // 正在加入的码（行级 loading）

  // 打开时拉「我的对接码」。
  useEffect(() => {
    if (!open) {
      setKeyword('');
      setSorter('default');
      return;
    }
    let dead = false;
    (async () => {
      try {
        const resp = await api('/admin/account/sms/haozhuma/my-uids');
        if (!dead) setMySet(new Set((resp.uids || []).map(u => u.uid)));
      } catch {
        if (!dead) setMySet(new Set()); // 拉不到就当全未加入（仍可手动加）
      }
    })();
    return () => { dead = true; };
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

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

  // 排序：default=已加入优先、置顶优先、库存降序；price=价格升序；stock=库存降序。
  const sorted = useMemo(() => {
    const arr = [...filtered];
    const mine = u => (mySet ? (mySet.has(u.uid) ? 1 : 0) : 0);
    if (sorter === 'price') arr.sort((a, b) => a.price - b.price);
    else if (sorter === 'stock') arr.sort((a, b) => b.stock - a.stock);
    else arr.sort((a, b) => (mine(b) - mine(a)) || (b.pinned - a.pinned) || (b.stock - a.stock));
    return arr;
  }, [filtered, sorter, mySet]);

  // 点行：已加入 → 直接选中；未加入 → 先加入（type=4）再选中。
  const pickRow = async (u) => {
    if (mySet && mySet.has(u.uid)) {
      onPick?.(u);
      onClose?.();
      return;
    }
    setAddingUid(u.uid);
    try {
      await api('/admin/account/sms/haozhuma/add-uid', {
        method: 'POST',
        body: JSON.stringify({ uid: u.uid }),
      });
      setMySet(current => new Set(current).add(u.uid));
      message.success(`已加入 ${u.uid} 并选中（官方 API 现在能用它取号了）`);
      onPick?.(u);
      onClose?.();
    } catch (err) {
      message.error(`加入失败：${err.message}（未选中）`);
    } finally {
      setAddingUid('');
    }
  };

  const columns = [
    {
      title: '',
      key: 'status',
      width: 64,
      render: (_, u) => {
        if (mySet == null) return null;
        return mySet.has(u.uid)
          ? <Tag color="green" style={{ marginRight: 0 }}>已加入</Tag>
          : <Tag style={{ marginRight: 0, color: '#999', borderColor: '#ddd' }}>未加入</Tag>;
      },
    },
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
      width: 160,
      render: (_, u) => (
        <Space size={4} wrap>
          {(u.isps || []).slice(0, 4).map(i => <Tag key={i} style={{ marginRight: 0 }}>{i}</Tag>)}
        </Space>
      ),
    },
    {
      title: '号段',
      key: 'segment',
      width: 96,
      render: (_, u) => u.segment_type && u.segment_type !== '未知号段'
        ? <Tag color="orange" style={{ marginRight: 0 }}>{u.segment_type}</Tag>
        : <Text type="secondary">—</Text>,
    },
    {
      title: '最近更新',
      dataIndex: 'updated_at',
      key: 'updated_at',
      width: 146,
      render: t => <Text type="secondary" style={{ fontSize: 12 }}>{t || '—'}</Text>,
    },
    {
      title: '',
      key: 'op',
      width: 96,
      render: (_, u) => (
        <Button
          type="link"
          size="small"
          icon={<PlusOutlined />}
          loading={addingUid === u.uid}
          onClick={e => { e.stopPropagation(); pickRow(u); }}
        >
          {mySet != null && mySet.has(u.uid) ? '选中' : '加入并选中'}
        </Button>
      ),
    },
  ];

  return (
    <Modal
      title={currentUid ? `选择对接码（当前 ${currentUid}）` : '选择对接码'}
      open={open}
      onCancel={onClose}
      footer={null}
      width={980}
      styles={{ body: { paddingTop: 12 } }}
    >
      <Space style={{ width: '100%', marginBottom: 4 }} direction="vertical" size={6}>
        <Text type="secondary" style={{ fontSize: 12 }}>
          官方取号 API 只认<b>已加入账户</b>的对接码：点「加入并选中」会先在豪猪侧加入该码（绿色「已加入」），再回填表单。
        </Text>
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
          <Radio.Button value="default">已加入/置顶优先</Radio.Button>
          <Radio.Button value="price">价格从低到高</Radio.Button>
          <Radio.Button value="stock">库存从高到低</Radio.Button>
        </Radio.Group>
      </Space>
      <div style={{ maxHeight: 440, overflowY: 'auto' }}>
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
            rowClassName={u => (mySet != null && mySet.has(u.uid) ? 'uid-row-mine' : '')}
            onRow={u => ({
              onClick: () => pickRow(u),
              style: { cursor: 'pointer' },
            })}
          />
        )}
      </div>
    </Modal>
  );
}
