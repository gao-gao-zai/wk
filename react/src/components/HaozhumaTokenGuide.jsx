import React from 'react';
import { Alert, Collapse, Typography } from 'antd';

const command = "console.log(localStorage.getItem('qs_web_token'))";

export default function HaozhumaTokenGuide() {
  return <Collapse size="small" items={[{
    key: 'guide', label: '如何获取豪猪 Web 登录令牌？', children: <>
      <Typography.Paragraph>先在浏览器打开 <a href="https://h5.haozhuma.com" target="_blank" rel="noopener noreferrer">豪猪网页版</a> 并完成登录。以下操作都在豪猪网页的开发者工具中进行。</Typography.Paragraph>
      <Typography.Paragraph><strong>方法一：控制台读取</strong><br />按 F12（Mac：⌘⌥I），进入 Console（控制台），粘贴并执行下面代码。它只在本地打印令牌，不发送请求，也不修改网站数据。</Typography.Paragraph>
      <Typography.Paragraph copyable={{ text: command }}><code>{command}</code></Typography.Paragraph>
      <Typography.Paragraph>复制输出的令牌内容（不要带引号），粘贴到本页上方输入框，点击“验证并使用令牌”。若输出 null，请先确认已登录，并确认控制台所在页面是 h5.haozhuma.com。</Typography.Paragraph>
      <Typography.Paragraph><strong>方法二：开发者工具手动查看</strong><br />按 F12 → Application（应用程序，Firefox 为 Storage/存储）→ Local Storage（本地存储）→ https://h5.haozhuma.com。找到键 qs_web_token，复制其 Value（值），粘贴到本页输入框，再点击“验证并使用令牌”。</Typography.Paragraph>
      <Alert type="info" showIcon message="这是网页登录令牌，与接码 API Token 分开使用。令牌只保存在服务内存，重启后需重新粘贴；失效时重新登录豪猪获取。请勿分享令牌或包含令牌的截图。" />
    </>
  }]} />;
}
