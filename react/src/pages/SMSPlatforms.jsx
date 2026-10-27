import React, { useEffect, useState } from 'react';
import { Alert, Button, Card, Collapse, Input, Modal, Popconfirm, Select, Space, Tag, Typography, message } from 'antd';
import { SettingOutlined, ReloadOutlined } from '@ant-design/icons';
import { platformNames, readyPlatforms } from '../lib/autoEnroll.js';
const { Text } = Typography;
const Field = ({ label, children, wide }) => <div className={`enroll-field ${wide ? 'enroll-field-wide' : ''}`}><label>{label}</label>{children}</div>;

function ProfileEditor({ provider, settings, api, onSave, onSaved, onRefresh, onAdvanced, busy, onDirtyChange, onBusyChange }) {
  const hao = provider === 'haozhuma';
  const auth = settings.platform_auth?.haozhuma || (settings.provider !== 'ecode' ? settings.auth : {}) || {};
  const [method, setMethod] = useState(auth.mode === 'userpass' ? 'userpass' : 'token');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [pass, setPass] = useState('');
  const [sid, setSid] = useState(settings.sid || '');
  const [keyword, setKeyword] = useState(settings.ecode_keyword || '');
  const [base, setBase] = useState(settings.ecode_base || 'https://api.eomsg.com/zc/data.php');
  const [card, setCard] = useState(settings.ecode_card_type || '全部');
  const [province, setProvince] = useState(settings.ecode_province || '');
  const [session, setSession] = useState('');
  const [saving, setSaving] = useState(false);
  const [checking, setChecking] = useState(false);
  const [result, setResult] = useState(null);
  const dirty = !!(session || token || user || pass || (hao ? sid !== (settings.sid || '') : keyword !== (settings.ecode_keyword || '') || base !== (settings.ecode_base || 'https://api.eomsg.com/zc/data.php') || card !== (settings.ecode_card_type || '全部') || province !== (settings.ecode_province || '')));
  useEffect(() => { onDirtyChange(dirty); }, [dirty, onDirtyChange]);
  useEffect(() => { onBusyChange(saving || checking); return () => onBusyChange(false); }, [saving, checking, onBusyChange]);
  useEffect(() => { setResult(null); }, [method, token, user, pass, sid, keyword, base, card, province]);
  const goAdvanced = () => {
    if (dirty) Modal.confirm({title:'有未保存的平台修改',content:'进入高级设置会丢弃当前平台草稿。',okText:'丢弃并继续',cancelText:'继续编辑',onOk:()=>onAdvanced('haozhuma')});
    else onAdvanced('haozhuma');
  };
  const credentials = () => {
    if (!hao || method === 'token') return token.trim() ? { token: token.trim(), user: '', pass: '' } : {};
    return { ...(user.trim() ? { user: user.trim() } : {}), ...(pass ? { pass } : {}) };
  };
  const check = async () => {
    setChecking(true); setResult(null);
    try {
      const body = { ...credentials(), ...(!hao ? { ecode_base: base.trim(), ecode_keyword: keyword.trim() } : {}) };
      const r = await api(`/admin/account/sms/platforms/${provider}/verify`, { method: 'POST', body: JSON.stringify(body) });
      setResult({ ok: true, text: `连接正常，账户余额 ¥${Number(r.balance).toFixed(2)}` });
    } catch (e) { setResult({ ok: false, text: e.message }); }
    finally { setChecking(false); }
  };
  const save = async () => {
    if (hao && !/^\d+$/.test(sid.trim())) { message.error('请填写豪猪数字项目 ID'); return; }
    if (!hao && !keyword.trim()) { message.error('请填写 E码云短信关键词'); return; }
    setSaving(true); setResult(null);
    try {
      const patch = { profile: provider, ...credentials(), ...(hao ? { sid: sid.trim() } : { ecode_keyword: keyword.trim(), ecode_base: base.trim(), ecode_card_type: card, ecode_province: province.trim() }) };
      const r = await onSave(patch);
      if (!r) return;
      message.success(`${platformNames[provider]}配置已保存`); onSaved();
    } catch (e) { setResult({ ok: false, text: e.message }); }
    finally { setSaving(false); }
  };
  const saveSession = async () => {
    if (!session.trim()) { message.error('请先粘贴豪猪 H5 会话'); return; }
    setSaving(true);
    try { await api('/admin/account/sms/haozhuma/h5-session', { method: 'POST', body: JSON.stringify({ session: session.trim() }) }); message.success('豪猪 H5 会话已保存'); setSession(''); onRefresh?.(); }
    catch (e) { setResult({ ok: false, text: e.message }); }
    finally { setSaving(false); }
  };
  return <Space direction="vertical" size={20} style={{ width: '100%' }}>
    <div className="enroll-settings-note">只修改{platformNames[provider]}配置，另一平台的凭据会保留。密钥留空使用已保存值；验证连接也会在服务端复用保存的凭据。</div>
    <fieldset disabled={busy || saving || checking} style={{ border: 0, margin: 0, padding: 0, width: '100%' }}>
      <div className="enroll-profile-fields">
        {hao && <Field label="鉴权方式" wide><Select aria-label="豪猪鉴权方式" value={method} disabled={busy || saving || checking} onChange={v => { setMethod(v); setToken(''); setUser(''); setPass(''); setResult(null); }} options={[{ value: 'token', label: 'API Token' }, { value: 'userpass', label: 'API 账号和密码（可自动重登）' }]} /></Field>}
        {hao && method === 'userpass' ? <>
          <Field label="API 账号"><Input aria-label="豪猪 API 账号" autoComplete="off" value={user} onChange={e => setUser(e.target.value)} placeholder={auth.user ? `已保存 ${auth.user}，留空保留` : '填写 API 账号'} /></Field>
          <Field label="API 密码"><Input.Password aria-label="豪猪 API 密码" autoComplete="new-password" value={pass} onChange={e => setPass(e.target.value)} placeholder={auth.has_pass ? '已保存，留空保留' : '填写 API 密码'} /></Field>
        </> : <Field label={`${platformNames[provider]} API Token`} wide><Input.Password aria-label={`${platformNames[provider]} API Token`} autoComplete="new-password" value={token} onChange={e => setToken(e.target.value)} placeholder={settings.saved_platforms?.[provider] ? '已保存，留空保留' : '粘贴该平台的 API Token'} /></Field>}
        {hao ? <Field label="豪猪项目 ID" wide><Input aria-label="豪猪项目 ID" value={sid} onChange={e => setSid(e.target.value)} placeholder="例如 52283" /></Field> : <>
          <Field label="短信关键词" wide><Input aria-label="E码云短信关键词" value={keyword} onChange={e => setKeyword(e.target.value)} placeholder="例如 腾讯科技" /></Field>
          <Field label="API 地址" wide><Input aria-label="E码云 API 地址" value={base} onChange={e => setBase(e.target.value)} /></Field>
          <Field label="卡类型"><Select aria-label="E码云卡类型" value={card} disabled={busy || saving || checking} onChange={setCard} options={['全部', '实卡', '虚卡'].map(value => ({ value, label: value }))} /></Field>
          <Field label="归属省份"><Input aria-label="E码云归属省份" value={province} onChange={e => setProvince(e.target.value)} placeholder="留空不限" /></Field>
        </>}
      </div>
      <div className="enroll-actions"><Button type="primary" loading={saving} disabled={busy || checking} onClick={save}>保存{platformNames[provider]}配置</Button><Button loading={checking} disabled={busy || saving} onClick={check}>验证连接</Button></div>
    </fieldset>
    {result && <Alert type={result.ok ? 'success' : 'error'} showIcon message={result.text} />}
    {hao && <fieldset disabled={busy || saving || checking} style={{border:0, margin:0, padding:0, width:'100%'}}><Collapse items={[{ key: 'h5', label: `豪猪增强功能 · ${settings.h5?.has && !settings.h5?.expired ? '会话已配置' : '可选'}`, children: <Space direction="vertical" style={{ width: '100%' }}>
      <Text type="secondary">仅项目搜索、对接码选择和监控需要 H5 会话。普通手动加号不需要。</Text>
      <Input.Password aria-label="豪猪 H5 会话" value={session} onChange={e => setSession(e.target.value)} placeholder="粘贴豪猪 PHPSESSID" />
      <Space wrap><Button loading={saving} disabled={busy} onClick={saveSession}>保存 H5 会话</Button>
      {settings.h5?.has && <Popconfirm title="清空豪猪 H5 会话？" description="项目搜索与监控将不可用，普通接码不受影响。" onConfirm={async () => { await api('/admin/account/sms/haozhuma/h5-session',{method:'POST',body:JSON.stringify({session:''})});onRefresh?.(); }}><Button disabled={busy}>清空会话</Button></Popconfirm>}</Space>
      <Button disabled={busy} onClick={goAdvanced}>项目与对接码高级设置</Button>
      <Popconfirm title="释放豪猪账户全部占用号码？" description="包括本服务账本以外的占用号。请确认没有其它任务在使用该账户。" onConfirm={async () => { try {await api('/admin/account/sms/release-all',{method:'POST',body:'{}'});message.success('平台占用号码已释放');onRefresh?.();}catch(e){setResult({ok:false,text:e.message});} }}><Button danger disabled={busy}>释放豪猪账户全部占用号码</Button></Popconfirm>
    </Space> }]} /></fieldset>}
  </Space>;
}

export default function SMSPlatforms({ settings, running, api, onSave, onSaved, onAdvanced }) {
  const ready = readyPlatforms(settings);
  const [editing, setEditing] = useState(null);
  const [editorDirty, setEditorDirty] = useState(false);
  const [editorBusy, setEditorBusy] = useState(false);
  const [balances, setBalances] = useState({});
  const [checking, setChecking] = useState(null);
  const refreshBalance = async provider => {
    setChecking(provider);
    setBalances(cur => ({ ...cur, [provider]: { loading: true } }));
    try { const r = await api(`/admin/account/sms/platforms/${provider}/summary`); setBalances(cur => ({ ...cur, [provider]: { ok: true, value: r.balance, time: new Date().toLocaleTimeString() } })); }
    catch (e) { setBalances(cur => ({ ...cur, [provider]: { error: e.message } })); }
    finally { setChecking(null); }
  };
  return <>
    <Card title="接码平台" extra={<Text type="secondary">独立保存</Text>}>
      {['haozhuma', 'ecode'].map(provider => <div className="enroll-platform-summary" key={provider}>
        <div><Text strong>{platformNames[provider]}</Text><Tag style={{ marginLeft: 10 }} color={ready[provider] ? 'green' : 'default'}>{ready[provider] ? '已配置' : '待配置'}</Tag>
          <div className="enroll-muted" style={{ marginTop: 5 }}>{provider === 'haozhuma' ? (settings.sid ? `项目 ${settings.sid}` : '需要凭据和项目 ID') : (settings.ecode_keyword || '需要 Token 和短信关键词')}</div>
          {balances[provider]?.ok && <div className="enroll-muted">余额 ¥{Number(balances[provider].value).toFixed(2)} · {balances[provider].time}</div>}
          {balances[provider]?.error && <Text type="danger" style={{ fontSize: 12 }}>查询失败：{balances[provider].error}</Text>}
        </div>
        <Space size={4}><Button aria-label={`刷新${platformNames[provider]}余额`} type="text" icon={<ReloadOutlined />} disabled={!ready[provider] || checking !== null} loading={checking === provider} onClick={() => refreshBalance(provider)} /><Button size="small" icon={<SettingOutlined />} disabled={running} onClick={() => setEditing(provider)}>配置</Button></Space>
      </div>)}
      <div className="enroll-muted" style={{ marginTop: 16 }}>保存两家配置后，在任务中选择使用哪一家。余额仅在点击刷新时向平台查询。</div>
    </Card>
    <Modal title={editing ? `${platformNames[editing]}配置` : '平台配置'} width={680} open={!!editing} footer={null} destroyOnClose closable={!editorBusy} maskClosable={false} onCancel={() => { if(editorBusy)return; if(editorDirty)Modal.confirm({title:'有未保存的平台修改',content:'关闭后将丢弃这些改动。',okText:'丢弃并关闭',cancelText:'继续编辑',onOk:()=>setEditing(null)});else setEditing(null); }}>
      {editing && <ProfileEditor onDirtyChange={setEditorDirty} onBusyChange={setEditorBusy} key={editing} provider={editing} settings={settings} api={api} onSave={onSave} onRefresh={onSaved} onAdvanced={provider => { setEditing(null); onAdvanced(provider); }} busy={running} onSaved={() => { setBalances({}); setEditing(null); onSaved?.(); }} />}
    </Modal>
  </>;
}
