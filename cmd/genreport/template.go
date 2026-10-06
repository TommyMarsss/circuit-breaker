package main

// reportTemplate 是单文件回放报告模板。__META__ 与 __FRAMES__
// 在生成时被替换为内嵌 JSON，浏览器打开即可逐帧回放，无需任何服务。
const reportTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>熔断器状态回放</title>
<style>
  :root {
    --bg: #0f1420; --panel: #171e2e; --border: #2a3550; --text: #dbe4f5;
    --muted: #8b98b8; --closed: #34c98e; --open: #f2555a; --half: #f5a623;
    --accent: #5b9dff;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg); color: var(--text); font: 14px/1.6 -apple-system, "PingFang SC", "Helvetica Neue", sans-serif; padding: 20px; }
  h1 { font-size: 20px; margin-bottom: 4px; }
  .sub { color: var(--muted); margin-bottom: 16px; }
  .layout { display: grid; grid-template-columns: 1fr 340px; gap: 16px; max-width: 1280px; margin: 0 auto; }
  @media (max-width: 900px) { .layout { grid-template-columns: 1fr; } }
  .card { background: var(--panel); border: 1px solid var(--border); border-radius: 10px; padding: 14px 16px; }
  .badge { display: inline-block; padding: 3px 14px; border-radius: 999px; font-weight: 700; font-size: 15px; }
  .badge.closed { background: rgba(52,201,142,.15); color: var(--closed); border: 1px solid var(--closed); }
  .badge.open { background: rgba(242,85,90,.15); color: var(--open); border: 1px solid var(--open); }
  .badge.half-open { background: rgba(245,166,35,.15); color: var(--half); border: 1px solid var(--half); }
  .stats { display: grid; grid-template-columns: 1fr 1fr; gap: 6px 12px; margin-top: 12px; font-size: 13px; }
  .stats b { color: var(--accent); font-variant-numeric: tabular-nums; }
  .controls { display: flex; align-items: center; gap: 10px; margin: 14px 0; flex-wrap: wrap; }
  button, select { background: #223050; color: var(--text); border: 1px solid var(--border); border-radius: 6px; padding: 6px 14px; cursor: pointer; font-size: 13px; }
  button:hover { background: #2b3d66; }
  input[type=range] { flex: 1; min-width: 160px; accent-color: var(--accent); }
  canvas { width: 100%; display: block; }
  .chart-label { color: var(--muted); font-size: 12px; margin: 10px 0 4px; }
  #log { max-height: 420px; overflow-y: auto; margin-top: 10px; font-size: 12.5px; }
  #log div { padding: 5px 8px; border-radius: 6px; border-left: 3px solid transparent; color: var(--muted); }
  #log div.cur { background: #223050; color: var(--text); border-left-color: var(--accent); }
  #log div.trans { border-left-color: var(--half); }
  #log div .t { color: var(--accent); font-variant-numeric: tabular-nums; margin-right: 8px; }
  .flash { animation: flash .8s ease-out; }
  @keyframes flash { 0% { box-shadow: 0 0 0 3px var(--half); } 100% { box-shadow: none; } }
  .legend { display: flex; gap: 16px; font-size: 12px; color: var(--muted); margin-top: 8px; flex-wrap: wrap; }
  .dot { display: inline-block; width: 10px; height: 10px; border-radius: 3px; margin-right: 5px; vertical-align: -1px; }
</style>
</head>
<body>
<div class="layout">
  <div>
    <h1>熔断器状态机回放</h1>
    <div class="sub" id="meta"></div>
    <div class="card">
      <div class="controls">
        <button id="play">▶ 播放</button>
        <button id="prev">‹ 上一帧</button>
        <button id="next">下一帧 ›</button>
        <select id="speed">
          <option value="1600">0.5×</option>
          <option value="800" selected>1×</option>
          <option value="400">2×</option>
          <option value="150">5×</option>
        </select>
        <input type="range" id="slider" min="0" value="0">
      </div>
      <div class="chart-label">状态时间线（绿=关闭 红=打开 黄=半开；竖线=状态切换）</div>
      <canvas id="timeline" height="64"></canvas>
      <div class="chart-label">滑动窗口错误率曲线（虚线=打开阈值；灰底=样本数未达最小门限）</div>
      <canvas id="chart" height="220"></canvas>
      <div class="legend">
        <span><span class="dot" style="background:var(--closed)"></span>关闭</span>
        <span><span class="dot" style="background:var(--open)"></span>打开</span>
        <span><span class="dot" style="background:var(--half)"></span>半开</span>
        <span><span class="dot" style="background:var(--accent)"></span>错误率曲线</span>
      </div>
    </div>
  </div>
  <div class="card" id="side">
    <div>当前状态：<span class="badge closed" id="stateBadge">closed</span></div>
    <div class="stats">
      <span>时刻</span><b id="sT">-</b>
      <span>窗口请求数</span><b id="sTotal">-</b>
      <span>窗口失败数</span><b id="sFail">-</b>
      <span>错误率</span><b id="sRate">-</b>
      <span>在飞探测</span><b id="sFlight">-</b>
      <span>连续成功</span><b id="sConsec">-</b>
    </div>
    <div id="log"></div>
  </div>
</div>
<script>
var META = __META__;
var FRAMES = __FRAMES__;
var COLORS = { closed: "#34c98e", open: "#f2555a", "half-open": "#f5a623" };
var idx = 0, playing = false, timer = null;
var t0 = FRAMES[0].t, t1 = FRAMES[FRAMES.length - 1].t;

document.getElementById("meta").textContent =
  "窗口 " + META.windowMs + "ms · 最小样本 " + META.minRequests +
  " · 错误率阈值 " + (META.errorThreshold * 100) + "% · 冷却 " + META.cooldownMs +
  "ms · 半开探测配额 " + META.maxProbes + " · 关闭需连续成功 " + META.successThreshold;

var slider = document.getElementById("slider");
slider.max = FRAMES.length - 1;

function x(t, w) { return 8 + (t - t0) / (t1 - t0) * (w - 16); }

function setupCanvas(id) {
  var c = document.getElementById(id);
  var dpr = window.devicePixelRatio || 1;
  var w = c.clientWidth, h = c.height;
  c.width = w * dpr; c.height = h * dpr;
  var ctx = c.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx: ctx, w: w, h: h };
}

function draw() {
  var f = FRAMES[idx];
  // 侧栏
  var badge = document.getElementById("stateBadge");
  badge.textContent = f.state;
  badge.className = "badge " + f.state;
  document.getElementById("sT").textContent = f.t + " ms";
  document.getElementById("sTotal").textContent = f.total;
  document.getElementById("sFail").textContent = f.fail;
  document.getElementById("sRate").textContent = (f.rate * 100).toFixed(1) + "%";
  document.getElementById("sFlight").textContent = f.flight;
  document.getElementById("sConsec").textContent = f.consec;
  if (f.trans) {
    var side = document.getElementById("side");
    side.classList.remove("flash"); void side.offsetWidth; side.classList.add("flash");
  }
  // 日志
  var log = document.getElementById("log");
  log.innerHTML = "";
  for (var i = 0; i <= idx; i++) {
    var d = document.createElement("div");
    d.className = (i === idx ? "cur " : "") + (FRAMES[i].trans ? "trans" : "");
    d.innerHTML = "<span class='t'>" + FRAMES[i].t + "ms</span>" + FRAMES[i].desc;
    log.appendChild(d);
  }
  log.scrollTop = log.scrollHeight;
  slider.value = idx;

  // 状态时间线
  var tl = setupCanvas("timeline"), c = tl.ctx;
  c.clearRect(0, 0, tl.w, tl.h);
  for (i = 0; i < FRAMES.length; i++) {
    var a = FRAMES[i], b = FRAMES[i + 1];
    var x0 = x(a.t, tl.w), x1 = b ? x(b.t, tl.w) : tl.w - 8;
    c.fillStyle = COLORS[a.state] + "33";
    c.fillRect(x0, 10, x1 - x0, tl.h - 20);
    c.fillStyle = COLORS[a.state];
    c.fillRect(x0, tl.h / 2 - 3, Math.max(x1 - x0, 1.5), 6);
    if (a.trans) {
      c.fillStyle = "#fff";
      c.fillRect(x0 - 1, 4, 2, tl.h - 8);
    }
  }
  var cx = x(f.t, tl.w);
  c.strokeStyle = "#fff"; c.lineWidth = 1.5;
  c.beginPath(); c.moveTo(cx, 0); c.lineTo(cx, tl.h); c.stroke();

  // 错误率曲线
  var ch = setupCanvas("chart"); c = ch.ctx;
  c.clearRect(0, 0, ch.w, ch.h);
  var padT = 14, padB = 24, plotH = ch.h - padT - padB;
  function y(r) { return padT + (1 - r) * plotH; }
  // 样本数未达门限的灰底区间
  for (i = 0; i < FRAMES.length; i++) {
    if (FRAMES[i].total < META.minRequests) {
      var nx = FRAMES[i + 1] ? x(FRAMES[i + 1].t, ch.w) : ch.w - 8;
      c.fillStyle = "rgba(140,150,180,0.08)";
      c.fillRect(x(FRAMES[i].t, ch.w), padT, nx - x(FRAMES[i].t, ch.w), plotH);
    }
  }
  // 阈值虚线
  c.strokeStyle = "#f2555a"; c.setLineDash([6, 5]); c.lineWidth = 1;
  c.beginPath(); c.moveTo(8, y(META.errorThreshold)); c.lineTo(ch.w - 8, y(META.errorThreshold)); c.stroke();
  c.setLineDash([]);
  c.fillStyle = "#8b98b8"; c.font = "11px sans-serif";
  c.fillText("阈值 " + (META.errorThreshold * 100) + "%", 12, y(META.errorThreshold) - 5);
  c.fillText("0%", 12, y(0) + 12); c.fillText("100%", 12, y(1) + 4);
  // 阶梯曲线（截至当前帧）
  c.strokeStyle = "#5b9dff"; c.lineWidth = 2; c.beginPath();
  for (i = 0; i <= idx; i++) {
    var px = x(FRAMES[i].t, ch.w), py = y(FRAMES[i].rate);
    if (i === 0) c.moveTo(px, py);
    else { c.lineTo(px, y(FRAMES[i - 1].rate)); c.lineTo(px, py); }
  }
  c.stroke();
  // 状态切换高亮点
  for (i = 0; i <= idx; i++) {
    if (FRAMES[i].trans) {
      c.fillStyle = COLORS[FRAMES[i].state];
      c.beginPath(); c.arc(x(FRAMES[i].t, ch.w), y(FRAMES[i].rate), 5, 0, 7); c.fill();
      c.strokeStyle = "#fff"; c.lineWidth = 1.5;
      c.beginPath(); c.arc(x(FRAMES[i].t, ch.w), y(FRAMES[i].rate), 7, 0, 7); c.stroke();
    }
  }
  // 当前帧光标
  cx = x(f.t, ch.w);
  c.strokeStyle = "rgba(255,255,255,.7)"; c.lineWidth = 1.5;
  c.beginPath(); c.moveTo(cx, padT); c.lineTo(cx, ch.h - padB); c.stroke();
  c.fillStyle = "#fff";
  c.beginPath(); c.arc(cx, y(f.rate), 4, 0, 7); c.fill();
}

function step(d) {
  idx = Math.min(Math.max(idx + d, 0), FRAMES.length - 1);
  if (idx === FRAMES.length - 1) pause();
  draw();
}
function pause() { playing = false; clearInterval(timer); document.getElementById("play").textContent = "▶ 播放"; }
function play() {
  if (idx >= FRAMES.length - 1) idx = 0;
  playing = true;
  document.getElementById("play").textContent = "⏸ 暂停";
  var ms = parseInt(document.getElementById("speed").value, 10);
  timer = setInterval(function () { step(1); }, ms);
}
document.getElementById("play").onclick = function () { playing ? pause() : play(); };
document.getElementById("prev").onclick = function () { pause(); step(-1); };
document.getElementById("next").onclick = function () { pause(); step(1); };
document.getElementById("speed").onchange = function () { if (playing) { pause(); play(); } };
slider.oninput = function () { pause(); idx = parseInt(slider.value, 10); draw(); };
window.onresize = draw;
document.onkeydown = function (e) {
  if (e.key === "ArrowRight") { pause(); step(1); }
  if (e.key === "ArrowLeft") { pause(); step(-1); }
  if (e.key === " ") { e.preventDefault(); playing ? pause() : play(); }
};
draw();
</script>
</body>
</html>
`
