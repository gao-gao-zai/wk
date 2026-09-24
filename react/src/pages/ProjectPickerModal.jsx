import React, { useEffect, useRef, useState } from 'react';
import { Input, Modal, Spin, Tag, Typography } from 'antd';
import { SearchOutlined } from '@ant-design/icons';

const { Text } = Typography;

/**
 * ProjectPickerModal 浮窗式项目选择器（豪猪 H5 type=30）。
 *
 * 独立 Modal 而不是表单内嵌 AutoComplete 的原因：
 *   - 抽屉里内嵌的下拉受 560px 宽度挤压，项目名+ID+说明排不开；
 *   - 搜索是独立动作（输入关键词 → 显式回车/点搜索），不是每击键联想，
 *     放浮窗里操作路径更清晰：点「选择项目」→ 搜索 → 点选 → 自动回填；
 *   - 浮窗列表可以给每行更多展示位（项目名、数字 ID、hex 标识、对接码
 *     入口按钮），内嵌下拉给不了。
 *
 * 选中回调回传完整对象 { project_id, sid, name }——调用方同时拿到
 * 数字 ID（写配置用）和 hex sid（拉对接码用），映射在调用方维护。
 */
export default function ProjectPickerModal({ open, onClose, api, onPick }) {
  const [keyword, setKeyword] = useState('');
  const [projects, setProjects] = useState(null); // null=未搜索
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const timer = useRef(null);

  // 每次打开重置（不残留上次的搜索词与结果）。
  useEffect(() => {
    if (open) {
      setKeyword('');
      setProjects(null);
      setError('');
    }
  }, [open]);

  const search = async () => {
    const q = keyword.trim();
    if (!q) return;
    setLoading(true);
    setError('');
    try {
      const resp = await api(`/admin/account/sms/haozhuma/projects?q=${encodeURIComponent(q)}`);
      setProjects(resp.projects || []);
    } catch (err) {
      setError(err.message);
      setProjects(null);
    } finally {
      setLoading(false);
    }
  };

  const pick = (p) => {
    onPick?.(p);
    onClose?.();
  };

  return (
    <Modal
      title="选择项目"
      open={open}
      onCancel={onClose}
      footer={null}
      width={640}
      styles={{ body: { paddingTop: 12 } }}
    >
      <Input.Search
        value={keyword}
        onChange={e => setKeyword(e.target.value)}
        onSearch={search}
        placeholder="输入项目关键词，如：腾讯、微信、抖音"
        enterButton="搜索"
        prefix={<SearchOutlined />}
        autoFocus
        allowClear
        size="large"
      />
      <div style={{ marginTop: 12, maxHeight: 420, overflowY: 'auto' }}>
        {loading && <div style={{ textAlign: 'center', padding: 32 }}><Spin tip="搜索中…" /></div>}
        {!loading && error && <Text type="danger" style={{ display: 'block', padding: 16 }}>{error}</Text>}
        {!loading && !error && projects == null && (
          <Text type="secondary" style={{ display: 'block', padding: '24px 8px' }}>
            输入关键词搜索豪猪项目库。常用：腾讯（微信/小程序验证码）、抖音、微博。
          </Text>
        )}
        {!loading && !error && projects != null && projects.length === 0 && (
          <Text type="secondary" style={{ display: 'block', padding: '24px 8px' }}>
            没有匹配的项目，换个关键词再试（用项目名里的核心词，如"腾讯"而不是"腾讯科技轩辕传奇"）。
          </Text>
        )}
        {!loading && !error && projects != null && projects.length > 0 && (
          projects.map(p => (
            <div
              key={p.sid}
              onClick={() => pick(p)}
              style={{
                padding: '10px 12px', borderBottom: '1px solid #f0f0f0', cursor: 'pointer',
                borderRadius: 6,
              }}
              onMouseEnter={e => { e.currentTarget.style.background = '#f5f7fa'; }}
              onMouseLeave={e => { e.currentTarget.style.background = ''; }}
            >
              <Text strong style={{ fontSize: 14 }}>
                {p.project_id ? `【${p.project_id}】` : ''}
                {(p.name || '').replace(/^【\d+】/, '').replace(/\s*\[[0-9a-f]+\]$/, '')}
              </Text>
              {p.project_id
                ? <Tag style={{ marginLeft: 8 }}>ID {p.project_id}</Tag>
                : <Tag color="warning" style={{ marginLeft: 8 }}>未能解析项目 ID（选它后需手填）</Tag>}
            </div>
          ))
        )}
      </div>
    </Modal>
  );
}
