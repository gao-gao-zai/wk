import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Checkbox, Empty, Input, message, Modal, Radio, Space, Spin, Table, Tag, Typography } from 'antd';
import { SearchOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * UIDPickerModal 浮窗式对接码选择器（豪猪 H5 type=8/3/4），多选轮换池版。
 *
 * 交互：每行一个勾选框，勾上即加入「待选集」；点「加入 N 个并确定」时，
 * 把未加入账户的码先经后端在豪猪侧加入（type=4，官方 API 只认已加入的
 * 码），然后整体回填表单的 uids 轮换池字段。已勾选但未保存的行淡蓝底。
 *
 * 「加入」语义（关键业务规则）：官方取号 API 只认**已加入账户**的对接码
 * （市场挂牌 ≠ 拥有）。已加入的行标绿；未加入的勾选后保存时自动加入。
 * 取消勾选并保存 → 上层 diff 后自动从账户移出（type=41）。
 */
export default function UIDPickerModal({ open, onClose, uidItems, loading, selectedUids, onConfirm, api }) {
  const [keyword, setKeyword] = useState('');
  const [sorter, setSorter] = useState('default');
  const [mySet, setMySet] = useState(null); // Set<string> 已加入账户的码；null=未加载
  const [pending, setPending] = useState(null); // 勾选集（null=未动过，跟随 selectedUids）
  const [adding, setAdding] = useState(false);

  // 打开时初始化。
  useEffect(() => {
    if (!open) {
      setKeyword('');
      setSorter('default');
      setPending(null);
      return;
    }
    setPending(null); // 跟随外部 selectedUids
    let dead = false;
    (async () => {
      try {
        const resp = await api('/admin/account/sms/haozhuma/my-uids');
        if (!dead) setMySet(new Set((resp.uids || []).map(u => u.uid)));
      } catch {
        if (!dead) setMySet(new Set());
      }
    })();
    return () => { dead = true; };
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

  // 勾选集：未动过 = 表单现值；动过 = pending。
  const checked = pending ?? (selectedUids || []);
  const checkedSet = useMemo(() => new Set(checked), [checked]);
  const dirty = pending !== null
    && [...checked].sort().join('\n') !== [...(selectedUids || [])].sort().join('\n');

  const toggle = (uid) => {
    const next = new Set(checked);
    if (next.has(uid)) next.delete(uid);
    else next.add(uid);
    setPending([...next]);
  };

  // 过滤：关键词匹配 uid / 运营商 / 号段类型 / 省份。
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

  // 排序：勾选优先 → 已加入优先 → 置顶 → 库存；price/stock 另两档。
  const sorted = useMemo(() => {
    const arr = [...filtered];
    const mine = u => (mySet ? (mySet.has(u.uid) ? 1 : 0) : 0);
    const picked = u => (checkedSet.has(u.uid) ? 1 : 0);
    if (sorter === 'price') arr.sort((a, b) => a.price - b.price);
    else if (sorter === 'stock') arr.sort((a, b) => b.stock - a.stock);
    else arr.sort((a, b) => (picked(b) - picked(a)) || (mine(b) - mine(a)) || (b.pinned - a.pinned) || (b.stock - a.stock));
    return arr;
  }, [filtered, sorter, mySet, checkedSet]);

  // 确定：未加入账户的勾选项先加入（type=4），全部成功才回填。
  const confirm = async () => {
    if (mySet) {
      const toAdd = checked.filter(u => !mySet.has(u));
      if (toAdd.length > 0) {
        setAdding(true);
        try {
          for (const uid of toAdd) {
            await api('/admin/account/sms/haozhuma/add-uid', {
              method: 'POST',
              body: JSON.stringify({ uid }),
            });
          }
          setMySet(cur => {
            const next = new Set(cur);
            toAdd.forEach(u => next.add(u));
            return next;
          });
          message.success(`已把 ${toAdd.length} 个码加入豪猪账户（官方 API 现在能用它们取号）`);
        } catch (err) {
          message.error(`加入失败：${err.message}（已勾选未加入的码没有回填）`);
          setAdding(false);
          return;
        } finally {
          setAdding(false);
        }
      }
    }
    onConfirm?.(checked);
    onClose?.();
  };

  const columns = [
    {
      title: '',
      key: 'check',
      width: 40,
      render: (_, u) => (
        <Checkbox
          checked={checkedSet.has(u.uid)}
          onClick={e => { e.stopPropagation(); toggle(u.uid); }}
        />
      ),
    },
    {
      title: '账户',
      key: 'mine',
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
        <Text copyable={{ text: uid }}>{uid}</Text>
      ),
    },
    {
      title: '价格',
      dataIndex: 'price',
      key: 'price',
      width: 76,
      align: 'right',
      render: p => <Text>{Number(p).toFixed(2)}元</Text>,
    },
    {
      title: '库存',
      dataIndex: 'stock',
      key: 'stock',
      width: 66,
      align: 'right',
      render: s => <Text type={s > 20 ? 'success' : s > 0 ? 'warning' : 'danger'}>{s >= 0 ? s : '未知'}</Text>,
    },
    {
      title: '运营商',
      key: 'isps',
      width: 150,
      render: (_, u) => (
        <Space size={4} wrap>
          {(u.isps || []).slice(0, 4).map(i => <Tag key={i} style={{ marginRight: 0 }}>{i}</Tag>)}
        </Space>
      ),
    },
    {
      title: '号段',
      key: 'segment',
      width: 90,
      render: (_, u) => u.segment_type && u.segment_type !== '未知号段'
        ? <Tag color="orange" style={{ marginRight: 0 }}>{u.segment_type}</Tag>
        : <Text type="secondary">—</Text>,
    },
  ];

  return (
    <Modal
      title={`选择对接码（已勾选 ${checked.length} 个）`}
      open={open}
      onCancel={onClose}
      width={1000}
      styles={{ body: { paddingTop: 12 } }}
      footer={
        <Space style={{ width: '100%', justifyContent: 'space-between' }}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            勾选多个组成轮换池：取号逐个轮着用，失效自动移出。官方 API 只认<b>已加入</b>账户的码（绿色）——未加入的勾选后点确定会自动加入。
          </Text>
          <Space>
            <Button onClick={onClose}>取消</Button>
            <Button type="primary" loading={adding} disabled={!dirty && mySet == null} onClick={confirm}>
              {mySet && checked.filter(u => !mySet.has(u)).length > 0
                ? `加入 ${checked.filter(u => !mySet.has(u)).length} 个并确定`
                : `确定（${checked.length} 个）`}
            </Button>
          </Space>
        </Space>
      }
    >
      <Space style={{ width: '100%', marginBottom: 8 }} direction="vertical" size={6}>
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
          <Radio.Button value="default">勾选/已加入优先</Radio.Button>
          <Radio.Button value="price">价格从低到高</Radio.Button>
          <Radio.Button value="stock">库存从高到低</Radio.Button>
        </Radio.Group>
      </Space>
      <div style={{ maxHeight: 420, overflowY: 'auto' }}>
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
            rowClassName={u => (checkedSet.has(u.uid) ? 'uid-row-picked' : (mySet != null && mySet.has(u.uid) ? 'uid-row-mine' : ''))}
            onRow={u => ({
              onClick: () => toggle(u.uid),
              style: { cursor: 'pointer' },
            })}
          />
        )}
      </div>
    </Modal>
  );
}
