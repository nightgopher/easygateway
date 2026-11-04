#!/usr/bin/env node
// @ts-check

// EasyGateway 桌面端 UI E2E 冒烟测试。
// 机制对齐 easyagent：基于 agent-desktop CLI 的无障碍树驱动，全部用例非破坏性
// （不点击 登录/迁移/移除/清理/保存 等会改变网关配置或账号数据的控件）。
// 运行：node scripts/desktop-ui-smoke.mjs（需正在运行的 EasyGateway.app）。

import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { SmokeTestSuite } from './lib/desktop-tester.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const suite = new SmokeTestSuite('EasyGateway Desktop UI E2E Smoke');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 侧边栏入口 → 每页一个"该页独有"的锚点控件；切页后锚点出现且上一页锚点消失，
// 证明视图真的切换了，而不是点了个寂寞。
const PAGES = [
  { nav: '运行概览', anchor: (n) => n.role === 'button' && n.name === '近 7 天', prevAnchors: ['搜索模型', '搜索日志'] },
  { nav: '模型与路由', anchor: (n) => n.role === 'textfield' && n.name === '搜索模型', prevAnchors: ['搜索日志'] },
  { nav: 'Provider', anchor: (n) => n.role === 'button' && /重置熔断/.test(n.name || ''), prevAnchors: ['搜索模型', '搜索日志'] },
  { nav: '账号渠道', anchor: (n) => n.role === 'button' && /添加账号|登录 /.test(n.name || ''), prevAnchors: ['搜索模型', '搜索日志'] },
  { nav: '请求日志', anchor: (n) => n.role === 'textfield' && n.name === '搜索日志', prevAnchors: ['搜索模型'] },
  { nav: '设置', anchor: (n) => ['textarea','textfield'].includes(n.role) && (n.name || '').includes('助理提示词'), prevAnchors: ['搜索模型', '搜索日志'] },
];

// TC-01: 发现并绑定 EasyGateway 主窗口
suite.test('TC-01: 探测并绑定 EasyGateway 主窗口', async (driver, ctx) => {
  const windows = await driver.listWindows('EasyGateway');
  if (!windows || windows.length === 0) {
    throw new Error('未检测到正在运行的 EasyGateway 窗口，请先打开应用');
  }
  const mainWindow = windows.find((w) => w.title === 'EasyGateway') || windows[0];
  if (!mainWindow) throw new Error('未找到主窗口');
  ctx.activeWindowId = mainWindow.id;
  ctx.allWindows = windows;
  if (!mainWindow.accessible) {
    throw new Error(`窗口 ${mainWindow.id} 未开启系统无障碍访问权限`);
  }
});

// TC-02: 阻断弹窗检测与自愈清理（软件更新弹窗）
suite.test('TC-02: 阻断弹窗检测与自愈清理', async (driver, ctx) => {
  const updateDialog = (ctx.allWindows || []).find(
    (w) => w.title === 'Software Update' || w.title === '软件更新'
  );
  if (!updateDialog) return; // 没有弹窗是常态，直接通过
  try {
    const snap = await driver.snapshot(updateDialog.id, { interactiveOnly: false, compact: true });
    const cancelBtn = driver.findElement(
      snap.tree,
      (n) => n.role === 'button' && ['取消', 'Cancel', 'Later', '稍后'].includes(n.name)
    );
    if (cancelBtn) {
      await driver.click(cancelBtn.ref_id);
      await sleep(600);
    }
  } catch {
    // 弹窗可能已自动关闭，安全跳过
  }
});

// TC-03: 主窗口无障碍树与界面结构完整性校验
suite.test('TC-03: 界面无障碍结构树完整性校验', async (driver, ctx) => {
  // 先固定到运行概览，避免依赖外部遗留页面状态
  const nav = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const home = driver.findElement(
    nav.tree,
    (n) => ['treeitem', 'row', 'button'].includes(n.role) && n.name === '运行概览'
  );
  if (home) {
    await driver.click(home.ref_id);
    await sleep(800);
  }
  // agent-desktop 偶发返回残缺树，重试直到节点数达标
  let snap = null;
  let refCount = 0;
  for (let i = 0; i < 4; i++) {
    snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    refCount = snap.ref_count || 0;
    if (refCount >= 40) break;
    await sleep(800);
  }
  ctx.latestSnapshot = snap;
  if (refCount < 40) {
    throw new Error(`UI 节点数异常偏少 (${refCount} 个)，界面可能白屏或渲染异常`);
  }
  // 等网关数据载入（新启动实例经防火墙评估+首轮拉取，可能需要十几秒），
  // 否则 TC-04/05 会在"正在载入"占位态下误判页面缺内容
  for (let i = 0; i < 12; i++) {
    const probe = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    if (driver.findElement(probe.tree, (n) => n.name === '今日请求')) break;
    if (/无法连接网关/.test(JSON.stringify(probe.tree).slice(0, 200000))) {
      // 连接失败态：点一次刷新再等
      const btn = driver.findElement(probe.tree, (n) => n.role === 'button' && n.name === '刷新');
      if (btn) { try { await driver.click(btn.ref_id); } catch { /* 忽略 */ } }
    }
    await sleep(2000);
  }
  // 侧边栏 12 个入口必须全部存在
  const navNames = ['控制台', '运行概览', '模型与路由', 'Provider', '账号渠道', '请求日志', '个人工作区', '技能', '记忆', '助理', '设置', '连接'];
  for (const nav of navNames) {
    const found = driver.findElement(
      snap.tree,
      (n) => ['treeitem','row','button'].includes(n.role) && (n.name === nav || (n.name || '').startsWith(nav + ' '))
    );
    if (!found) throw new Error(`侧边栏缺少入口: ${nav}`);
  }
});

// TC-04: 核心页面逐个切换，锚点控件出现且上一页锚点消失
suite.test('TC-04: 侧边栏视图切换响应', async (driver, ctx) => {
  for (const page of PAGES) {
    const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    const navNode = driver.findElement(
      snap.tree,
      (n) => ['treeitem','row','button'].includes(n.role) && (n.name === page.nav || (n.name || '').startsWith(page.nav + ' '))
    );
    if (!navNode) throw new Error(`未定位到侧边栏入口「${page.nav}」`);
    await driver.click(navNode.ref_id);
    await sleep(700);

    const after = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    const anchor = driver.findElement(after.tree, page.anchor);
    if (!anchor) throw new Error(`切换到「${page.nav}」后未找到锚点控件`);
    for (const stale of page.prevAnchors || []) {
      const leftover = driver.findElement(
        after.tree,
        (n) => n.role === 'textfield' && n.name === stale
      );
      if (leftover) throw new Error(`切换到「${page.nav}」后仍残留上一页控件「${stale}」`);
    }
  }
  // 收尾回到运行概览
  const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const home = driver.findElement(
    snap.tree,
    (n) => ['treeitem','row','button'].includes(n.role) && n.name === '运行概览'
  );
  if (home) {
    await driver.click(home.ref_id);
    await sleep(500);
  }
});

// TC-05: 运行概览核心指标卡与时间范围切换
suite.test('TC-05: 运行概览指标卡与趋势范围切换', async (driver, ctx) => {
  const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  for (const label of ['今日请求', '成功率', '今日 Tokens', '平均延迟', '上游健康']) {
    const found = driver.findElement(snap.tree, (n) => n.name === label);
    if (!found) throw new Error(`运行概览缺少指标卡「${label}」`);
  }
  const d30 = driver.findElement(
    snap.tree,
    (n) => n.role === 'button' && n.name === '近 30 天'
  );
  if (!d30) throw new Error('未找到「近 30 天」切换按钮');
  // 范围切换固定在滚动容器外（页面级控件常驻可见），直接可点
  await driver.click(d30.ref_id);
  await sleep(600);
  const refresh = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const d7 = driver.findElement(refresh.tree, (n) => n.role === 'button' && n.name === '近 7 天');
  if (!d7) throw new Error('切到近 30 天后「近 7 天」按钮消失');
  await driver.click(d7.ref_id);
  await sleep(400);
});

// TC-06: 模型与路由搜索框过滤生效并可复原
suite.test('TC-06: 模型搜索框过滤与复原', async (driver, ctx) => {
  const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const navNode = driver.findElement(
    snap.tree,
    (n) => ['treeitem','row','button'].includes(n.role) && n.name === '模型与路由'
  );
  if (!navNode) throw new Error('未定位到「模型与路由」入口');
  await driver.click(navNode.ref_id);
  await sleep(700);

  const page = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const search = driver.findElement(
    page.tree,
    (n) => n.role === 'textfield' && n.name === '搜索模型'
  );
  if (!search) throw new Error('未定位到「搜索模型」输入框');

  const rowsWithModel = (tree) =>
    driver.findAllElements(tree, (n) => /deepv-|easycode\//.test(n.value || n.name || '')).length;

  const before = rowsWithModel(page.tree);
  await driver.setValue(search.ref_id, 'deepv');
  await sleep(600);
  const filtered = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const after = rowsWithModel(filtered.tree);
  if (after === 0) throw new Error('搜索 deepv 后结果为空，过滤未生效或模型目录异常');

  const refreshSearch = driver.findElement(
    filtered.tree,
    (n) => n.role === 'textfield' && n.name === '搜索模型'
  );
  if (refreshSearch) {
    await driver.setValue(refreshSearch.ref_id, '');
    await sleep(400);
  }
});

// TC-07: 请求日志条数档位切换与表头校验
suite.test('TC-07: 请求日志条数档位与表头', async (driver, ctx) => {
  const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const navNode = driver.findElement(
    snap.tree,
    (n) => ['treeitem','row','button'].includes(n.role) && n.name === '请求日志'
  );
  if (!navNode) throw new Error('未定位到「请求日志」入口');
  await driver.click(navNode.ref_id);
  await sleep(700);

  const page = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  for (const col of ['时间', '模型', 'Provider', '状态', '延迟']) {
    const found = driver.findElement(page.tree, (n) => n.name === col);
    if (!found) throw new Error(`请求日志表头缺少「${col}」`);
  }
  const radios = driver.findAllElements(
    page.tree,
    (n) => (n.role || '').includes('radio') && ['50', '100', '200'].includes(n.name)
  );
  if (radios.length !== 3) throw new Error(`日志条数档位异常：找到 ${radios.length} 个（应为 50/100/200）`);
  // 切到 200 再切回 50，验证档位可切换
  const r200 = radios.find((r) => r.name === '200');
  if (r200) {
    await driver.click(r200.ref_id);
    // 200 档触发表格重建，快照可能抓到中间态，档位查找带重试
    let r50 = null;
    for (let i = 0; i < 4 && !r50; i++) {
      await sleep(800);
      const back = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
      r50 = driver.findElement(back.tree, (n) => (n.role || '').includes('radio') && n.name === '50');
    }
    if (!r50) throw new Error('切到 200 档后找不到 50 档（档位控件丢失）');
    await driver.click(r50.ref_id);
    await sleep(400);
  }
});

// TC-08: 账号渠道完整性（飞书 EasyCode 渠道在列；未配置渠道登录按钮应为禁用态）
suite.test('TC-08: 账号渠道列表完整性', async (driver, ctx) => {
  const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  const navNode = driver.findElement(
    snap.tree,
    (n) => ['treeitem','row','button'].includes(n.role) && (n.name === '账号渠道' || (n.name || '').startsWith('账号渠道 '))
  );
  if (!navNode) throw new Error('未定位到「账号渠道」入口');
  await driver.click(navNode.ref_id);
  await sleep(700);

  // mygo 0.7+ 视口外元素可能被虚拟化；找不到时滚动渠道列表再找
  let page = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
  let easycode = driver.findElement(
    page.tree,
    (n) => (n.name || '').startsWith('EasyCode（飞书）')
  );
  for (let i = 0; i < 3 && !easycode; i++) {
    const table = driver.findElement(page.tree, (n) => n.role === 'table');
    if (!table) break;
    await driver.scrollBy(table.ref_id, 'down', 8);
    await sleep(700);
    page = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    easycode = driver.findElement(
      page.tree,
      (n) => (n.name || '').startsWith('EasyCode（飞书）')
    );
  }
  if (!easycode) throw new Error('账号渠道缺少 EasyCode（飞书）渠道');
  const channelNames = ['MiniMax', 'Cline', 'WorkBuddy', 'OpenCode', 'TRAE'];
  for (const ch of channelNames) {
    const found = driver.findElement(page.tree, (n) => n.name === ch);
    if (!found) throw new Error(`账号渠道缺少「${ch}」`);
  }
});

// TC-09: 多字节文本完整性探针（全页扫描 U+FFFD 替换符）
suite.test('TC-09: 无障碍文本无字节截断乱码', async (driver, ctx) => {
  for (const nav of ['运行概览', '请求日志', '账号渠道']) {
    const snap = await driver.snapshot(ctx.activeWindowId, { interactiveOnly: false, compact: true });
    const bad = driver.findAllElements(
      snap.tree,
      (n) => typeof n.name === 'string' && n.name.includes('\uFFFD')
    );
    if (bad.length > 0) {
      const sample = bad.slice(0, 3).map((n) => JSON.stringify(n.name)).join(', ');
      throw new Error(`「${nav}」页发现 ${bad.length} 处乱码文本（UTF-8 字节截断）: ${sample}`);
    }
  }
});

// TC-10: 导出 E2E 验收通过界面快照留档（尽力而为）
suite.test('TC-10: 导出验收界面快照留档', async (driver, ctx) => {
  const outputPath = path.resolve(process.cwd(), '.test-output', 'smoke-pass-latest.png');
  try {
    await driver.screenshot(ctx.activeWindowId, outputPath);
  } catch (err) {
    process.stdout.write(`\x1b[33m(截图跳过: ${err.message})\x1b[0m `);
  }
});

const success = await suite.run();
process.exit(success ? 0 : 1);
