"use strict";

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

const DRIVE_LABELS = { 0: "FWD 前驱", 1: "RWD 后驱", 2: "AWD 四驱" };
const CLASS_LABELS = { 0: "D", 1: "C", 2: "B", 3: "A", 4: "S1", 5: "S2", 6: "X" };

const EVENT_GROUPS = [
  {
    title: "7 大引擎家族与动力传控",
    sub: "横跨 28–156 Hz 宽频段与 36%–135% 振幅差：美式 V8（30–72Hz · 135%）重低音突突 / V12 丝绸高转 / 万转转子高频蜂鸣 / 涡轮暴涨 / 轻盈小自吸 / 纯电静音",
    items: [
      { key: "engineIdle", name: "怠速呼吸与突突质感", desc: "美式 V8 ~15.6Hz 猛烈凸轮轴突突脉动（30–72Hz · 135%），小排量轻柔微震，V12 丝滑轻鸣，纯电完全静谧。" },
      { key: "engineRev", name: "引擎拉转与涡轮爆发", desc: "按 7 大引擎家族独立塑形频率、谐波与振幅（V8 比纯电强近 4 倍、转子频率高 3 倍）。" },
      { key: "gearShift", name: "升降挡机械顿挫", desc: "机械咬合瞬态撞击 + 低频推背余震，同步驱动右扳机换挡冲击。" },
      { key: "redline", name: "自学习红线与断油提醒", desc: "自动捕获真实峰值功率与断油转速，临近红线高频间歇爆震提醒。" },
    ],
  },
  {
    title: "路面起伏、多材质纹理与路肩",
    sub: "6 种截然不同的物理纹路：柏油微感 / 土路随机石粒 / 重砂石石块撞击 / 草雪软绵低频 / 随速路肩锯齿 / 涉水波浪",
    items: [
      { key: "roadUndulation", name: "全时路面起伏与悬架直驱", desc: "真一阶高通连续捕捉每一处坑洼接缝与坡道起伏，体现绵软或生硬悬架性格。" },
      { key: "roadSurface", name: "多材质路面纹理（土路/砂石/草雪/柏油）", desc: "土路独立左右随机石粒拍打、重砂石大石块撞击、草雪软绵低频涌动，区别一摸即知。" },
      { key: "curbRumble", name: "路肩与马路牙子（随速锯齿 + 纯几何识别）", desc: "随车速实时变频的“哒-哒-哒”机械锯齿冲击，左右握把严格定向定位。" },
    ],
  },
  {
    title: "高低速 360° 撞击、定向护栏、飞坡与涉水",
    sub: "3 km/h 低速碰擦即刻响应 · 左右侧撞与护栏 100% vs 0% 极致单侧隔离（静音对侧握把）",
    items: [
      { key: "collisionWall", name: "高低速 360° 全向碰撞与撞碎障碍物", desc: "3–80km/h 低速碰擦与高速重撞双层捕获；左右侧撞击纯单侧 125ms 静音对侧，左右立判。" },
      { key: "guardrail", name: "左右护栏三特征定向金属刮擦", desc: "物理墙面摩擦减速 + 超抓地法向力 + 方向性蹭墙识别，仅刮擦侧猛烈金属磨削、对侧纯静。" },
      { key: "jumpLanding", name: "大小飞坡两段式落地冲击", desc: "捕捉 ≥35ms 小中大飞坡，合成 20ms 脆响触地 + 低频回弹余震，按左右压缩加权。" },
      { key: "waterSplash", name: "入水飞溅冲击与涉水波浪阻力", desc: "入水瞬间定向强劲水花冲击，涉水行驶中持续慢频波浪起伏阻力。" },
    ],
  },
  {
    title: "四轮胎直驱：推头、甩尾、烧胎与重刹",
    sub: "FL/FR/RL/RR 四轮独立载荷加权，分别直驱左/右握把与左/右扳机（四轮漂移触发 0x9E 双扳机同步）",
    items: [
      { key: "understeer", name: "推头 · FL/FR 前轮独立搓地", desc: "左前轮推头→左握把+左扳机，右前轮推头→右握把+右/左扳机，清晰指示哪一侧前轮突破极限。" },
      { key: "oversteerDrift", name: "甩尾 / 漂移 / 起步烧胎 · RL/RR 独立横扫", desc: "左/右后轮载荷滑移直驱对应握把，油门甩尾→右扳机、手刹/刹车甩尾→左扳机、四轮漂移→双扳机。" },
      { key: "brakingAbs", name: "重刹 ABS 脉动与轮胎抱死", desc: "急刹防抱死在左扳机输出 10Hz 清晰液压脉动并按左右胎滑移加权，彻底抱死切尖锐抖动。" },
    ],
  },
];

const state = {
  mode: "entertainment",
  udpPort: 5300,
  switchingMode: false,
  exiting: false,
  nextConfig: null,
  activeCar: null,
  redlineEdited: false,
  archetypes: [],
  garage: [],
  devices: [],
  writing: false,
  proStatus: null,
  proPresets: [],
  proFilter: "全部",
  proHello: null,
  proLiveStatus: null,
  proSettings: null,
  proRec: null,
  mainSSE: null,
  proSSE: null,
  proRecTimer: 0,
  proSSERetryTimer: 0,
  rafId: 0,
  saveTimer: 0,
  ringN: 320,
  ringGL: new Uint8Array(320).fill(128),
  ringGR: new Uint8Array(320).fill(128),
  ringT: new Uint8Array(320).fill(128),
  ringPos: 0,
  latestSnap: null,
  lastImpactAt: -Infinity,
  lastImpactAngle: 0,
  lastImpactLabel: "",
  lighting: null,
  ledSaveTimer: 0,
};

function toast(msg, err = false) {
  const el = document.createElement("div");
  el.className = "toast" + (err ? " err" : "");
  el.textContent = msg;
  $("toast-host").appendChild(el);
  setTimeout(() => el.remove(), 3400);
}

async function api(url, opts) {
  const r = await fetch(url, { cache: "no-store", ...opts });
  const text = await r.text();
  let data = {};
  try { data = JSON.parse(text); } catch { data = { raw: text }; }
  if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
  return data;
}

// ==================== INITIALIZATION ====================

async function loadInitialState() {
  if (state.exiting) return;
  try {
    const s = await api("/api/state");
    if (state.exiting) return;
    state.mode = s.mode;
    state.udpPort = s.udpPort || 5300;
    state.nextConfig = s.nextConfig;
    state.activeCar = s.activeCar;
    lastCarOrdinalSeen = s.activeCar?.carOrdinal || 0;
    state.archetypes = s.archetypes || [];
    state.garage = s.garage || [];
    state.devices = s.devices || [];
    state.writing = !!s.writing;
    state.proStatus = s.proStatus;
    state.proPresets = s.proPresets || [];
    if (s.proHello) state.proHello = s.proHello;
    if (s.proLiveStatus) state.proLiveStatus = s.proLiveStatus;
    if (s.lighting) {
      state.lighting = s.lighting;
      syncLightingControls(s.lighting.config);
      updateLightingUI(s.lighting);
    }

    if (s.udpBindError) {
      $("alert-bar").hidden = false;
      $("alert-bar").textContent = "⚠ " + s.udpBindError;
      setPill("pill-udp", "bad", `UDP ${state.udpPort} 被占用`);
    } else {
      $("alert-bar").hidden = true;
      setPill("pill-udp", "ok", `UDP ${state.udpPort} 就绪`);
    }

    $("ent-dev-all").checked = !!s.includeAllHid;
    renderModeUI();
    renderEntDevices();
    renderNextConfigControls();
    renderArchetypes();
    renderGarage();
    updateCarCard(state.activeCar, true);
    renderProPresets();
    updateProDiagnosticsLink();
    if (state.mode === "pro") {
      if (state.proHello) applyProHello(state.proHello);
      if (state.proLiveStatus) applyProLiveStatus(state.proLiveStatus);
      connectProStream();
      refreshProSettings();
      fetchProHelloFallback();
    }
  } catch (e) {
    toast("无法连接 ForzaNext 服务：" + e.message, true);
  }
}

function setPill(id, cls, text) {
  const el = $(id);
  el.className = "pill " + (cls || "");
  el.innerHTML = `<i class="dot"></i>${esc(text)}`;
}

// ==================== MODE SWITCHING ====================

function renderModeUI() {
  const isEnt = state.mode === "entertainment";
  $("btn-mode-ent").classList.toggle("active", isEnt);
  $("btn-mode-pro").classList.toggle("active", !isEnt);
  $("ent-device-strip").hidden = !isEnt;
  $("view-entertainment").hidden = !isEnt;
  $("view-pro").hidden = isEnt;
}

async function switchMode(target) {
  if (state.exiting || state.switchingMode || target === state.mode) return;
  state.switchingMode = true;
  $("btn-mode-ent").disabled = true;
  $("btn-mode-pro").disabled = true;
  toast(target === "pro" ? "正在释放手柄句柄并拉起 V4 原版引擎..." : "正在停止 V4 并切换回 Next 娱乐引擎...");
  try {
    await api("/api/mode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: target }),
    });
    if (target !== "pro") {
      disconnectProStream();
      state.proHello = null;
    }
    await loadInitialState();
    toast(target === "pro" ? "已切换至专业模式（V4 原版引擎），请点击「开启 V4 反馈」" : "已切换至娱乐模式（Next 状态引擎）");
  } catch (e) {
    toast("模式切换失败：" + e.message, true);
  } finally {
    state.switchingMode = false;
    $("btn-mode-ent").disabled = false;
    $("btn-mode-pro").disabled = false;
  }
}

$("btn-mode-ent").onclick = () => switchMode("entertainment");
$("btn-mode-pro").onclick = () => switchMode("pro");

document.querySelectorAll(".tab-btn").forEach((btn) => {
  btn.onclick = () => {
    document.querySelectorAll(".tab-btn").forEach((b) => b.classList.toggle("active", b === btn));
    document.querySelectorAll(".tab-pane").forEach((p) => p.classList.toggle("active", p.id === "tab-" + btn.dataset.tab));
  };
});

// ==================== ENTERTAINMENT DEVICES ====================

function renderEntDevices() {
  const sel = $("ent-device-select");
  const prev = sel.value;
  sel.innerHTML = "";
  if (!state.devices.length) {
    const opt = document.createElement("option");
    opt.value = "-1";
    opt.textContent = "未检测到 33B 手柄（请确认手柄已连接并退出其他 Engine）";
    sel.appendChild(opt);
    $("ent-dev-start").disabled = true;
    $("ent-dev-stop").disabled = !state.writing;
    return;
  }
  state.devices.forEach((d) => {
    const opt = document.createElement("option");
    opt.value = String(d.id);
    opt.textContent = `${d.preferred ? "★ " : ""}${d.product} (${d.vid}:${d.pid} · 0x${d.usagePage})${d.writing ? " [输出中]" : ""}`;
    if (d.writing) opt.selected = true;
    sel.appendChild(opt);
  });
  if (!state.writing && state.devices.some((d) => String(d.id) === prev)) sel.value = prev;
  $("ent-dev-start").disabled = state.writing;
  $("ent-dev-stop").disabled = !state.writing;
}

$("ent-dev-refresh").onclick = async () => {
  try {
    const r = await api("/api/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "refresh", includeAll: $("ent-dev-all").checked }),
    });
    state.devices = r.devices || [];
    renderEntDevices();
    toast(`已发现 ${state.devices.length} 个 33B HID 设备`);
  } catch (e) { toast(e.message, true); }
};
$("ent-dev-all").onchange = () => $("ent-dev-refresh").click();

$("ent-dev-start").onclick = async () => {
  const id = Number($("ent-device-select").value);
  try {
    const r = await api("/api/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "open", id }),
    });
    state.writing = !!r.writing;
    state.devices = r.devices || state.devices;
    renderEntDevices();
    toast("Next 触觉输出已开启，可点下方对比试震按钮立判不同路面与多向撞击！");
  } catch (e) { toast(e.message, true); }
};

$("ent-dev-stop").onclick = async () => {
  try {
    const r = await api("/api/devices", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "close" }),
    });
    state.writing = false;
    state.devices = r.devices || state.devices;
    renderEntDevices();
    toast("已停止硬件输出");
  } catch (e) { toast(e.message, true); }
};

// ==================== ENTERTAINMENT CONFIG & TEST BENCH ====================

function scheduleSaveNextConfig() {
  clearTimeout(state.saveTimer);
  state.saveTimer = setTimeout(async () => {
    try {
      await api("/api/next/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(state.nextConfig),
      });
    } catch (e) {
      toast("保存配置失败：" + e.message, true);
    }
  }, 120);
}

async function triggerTest(eventId) {
  try {
    await api("/api/next/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ eventId }),
    });
  } catch (e) { toast("试震失败：" + e.message, true); }
}

document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-test]");
  if (btn) {
    triggerTest(btn.dataset.test);
    return;
  }
  const engBtn = e.target.closest("[data-eng-test]");
  if (engBtn) {
    try {
      const r = await api("/api/next/car", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: "previewArchetype", archetypeId: engBtn.dataset.engTest }),
      });
      state.activeCar = r.car;
      updateCarCard(r.car, true);
      toast(`引擎盲测：${r.car.label}（${Math.round(r.car.engineFreqBaseHz)}–${Math.round(r.car.engineFreqTopHz)}Hz · 振幅 ${Math.round(r.car.engineBedScale * 100)}%）`);
      triggerTest("engineRev");
    } catch (err) {
      toast("切换引擎试震失败：" + err.message, true);
    }
  }
});

document.querySelectorAll(".style-btn").forEach((btn) => {
  btn.onclick = async () => {
    clearTimeout(state.saveTimer);
    state.saveTimer = 0;
    try {
      const r = await api("/api/next/preset", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: btn.dataset.style }),
      });
      state.nextConfig = r.config;
      renderNextConfigControls();
      toast(`已应用风格配方：${r.config.profileName}`);
    } catch (e) { toast(e.message, true); }
  };
});

$("next-export").onclick = () => {
  if (!state.nextConfig) {
    toast("配置尚未加载完成", true);
    return;
  }
  const blob = new Blob([JSON.stringify(state.nextConfig, null, 2)], { type: "application/json" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = `ForzaNext_${state.nextConfig.profileName || "custom"}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
};
$("next-import-btn").onclick = () => $("next-import-file").click();
$("next-import-file").onchange = async (e) => {
  const f = e.target.files[0];
  if (!f) return;
  clearTimeout(state.saveTimer);
  state.saveTimer = 0;
  try {
    const obj = JSON.parse(await f.text());
    if (!obj.events) throw new Error("不是有效的 ForzaNext 娱乐模式配置");
    const r = await api("/api/next/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(obj),
    });
    state.nextConfig = r.config;
    renderNextConfigControls();
    toast("已导入娱乐模式配置");
  } catch (err) { toast(err.message, true); }
  e.target.value = "";
};

function renderNextConfigControls() {
  const c = state.nextConfig;
  if (!c) return;

  const gHost = $("global-controls");
  gHost.innerHTML = "";
  const gSliders = [
    { key: "masterGripGain", label: "握把总强度", min: 0, max: 1.6, step: 0.05, fmt: (v) => Math.round(v * 100) + "%" },
    { key: "masterTriggerGain", label: "扳机总强度", min: 0, max: 1.6, step: 0.05, fmt: (v) => Math.round(v * 100) + "%" },
    { key: "stereoSeparation", label: "左右轮立体声分离度", min: 0, max: 1.0, step: 0.05, fmt: (v) => Math.round(v * 100) + "%" },
    { key: "duckingStrength", label: "瞬态侧链闪避（让位清晰度）", min: 0, max: 0.85, step: 0.05, fmt: (v) => Math.round(v * 100) + "%" },
    { key: "antiFatigueClamp", label: "持续底振饱满度上限", min: 0.4, max: 1.2, step: 0.04, fmt: (v) => Math.round(v * 100) + "%" },
  ];
  gSliders.forEach((s) => {
    const box = document.createElement("div");
    box.className = "ctrl-box";
    box.innerHTML = `<div class="ctrl-head"><span>${s.label}</span><b class="mono">${s.fmt(c[s.key])}</b></div>
      <input type="range" min="${s.min}" max="${s.max}" step="${s.step}" value="${c[s.key]}">`;
    const inp = box.querySelector("input");
    const out = box.querySelector("b");
    inp.oninput = () => {
      c[s.key] = Number(inp.value);
      out.textContent = s.fmt(c[s.key]);
      scheduleSaveNextConfig();
    };
    gHost.appendChild(box);
  });
  [
    { key: "autoCarMatching", label: "一车一性格自动匹配（18套+悬挂反推）" },
    { key: "autoRedlineLearn", label: "真实红线/断油转速在线自学习" },
  ].forEach((t) => {
    const box = document.createElement("div");
    box.className = "ctrl-box";
    box.innerHTML = `<div class="ctrl-head"><span>${t.label}</span><input type="checkbox" class="switch" ${c[t.key] ? "checked" : ""}></div>`;
    const chk = box.querySelector("input");
    chk.onchange = () => { c[t.key] = chk.checked; scheduleSaveNextConfig(); };
    gHost.appendChild(box);
  });

  const eHost = $("event-sections");
  eHost.innerHTML = "";
  EVENT_GROUPS.forEach((grp) => {
    const sec = document.createElement("div");
    sec.className = "section-group";
    sec.innerHTML = `<div class="section-title"><h3>${grp.title}</h3><span class="muted">${grp.sub}</span></div><div class="event-grid"></div>`;
    const grid = sec.querySelector(".event-grid");

    grp.items.forEach((item) => {
      const ev = c.events[item.key];
      if (!ev) return;
      const card = document.createElement("div");
      card.className = "ev-card" + (ev.enabled ? "" : " disabled");
      card.dataset.evKey = item.key;

      const sharpText = (v) => v < 0.35 ? `圆润饱满 (${Math.round(v * 100)}%)` : v > 0.68 ? `生硬犀利 (${Math.round(v * 100)}%)` : `韧性均衡 (${Math.round(v * 100)}%)`;

      card.innerHTML = `
        <div class="ev-head">
          <div class="ev-title">
            <input type="checkbox" class="switch" ${ev.enabled ? "checked" : ""}>
            <strong>${item.name}</strong>
          </div>
          <button class="btn sm" data-test="${item.key}">试震</button>
        </div>
        <div class="ev-desc">${item.desc}</div>
        <div class="slider-item">
          <div class="sl-head"><span>事件强度</span><b class="mono v-int">${Math.round(ev.intensity * 100)}%</b></div>
          <input type="range" class="r-int" min="0" max="1.6" step="0.05" value="${ev.intensity}">
        </div>
        <div class="slider-item">
          <div class="sl-head"><span>触感性格：圆润饱满 ↔ 生硬犀利</span><b class="mono v-shp">${sharpText(ev.sharpness)}</b></div>
          <input type="range" class="r-shp" min="0" max="1" step="0.05" value="${ev.sharpness}">
        </div>
        <div class="slider-item">
          <div class="sl-head"><span>频段偏移（低沉 ↔ 清脆）</span><b class="mono v-frq">${ev.freqScale.toFixed(2)}x</b></div>
          <input type="range" class="r-frq" min="0.65" max="1.45" step="0.05" value="${ev.freqScale}">
        </div>`;

      const sw = card.querySelector(".switch");
      const rInt = card.querySelector(".r-int"), vInt = card.querySelector(".v-int");
      const rShp = card.querySelector(".r-shp"), vShp = card.querySelector(".v-shp");
      const rFrq = card.querySelector(".r-frq"), vFrq = card.querySelector(".v-frq");

      sw.onchange = () => { ev.enabled = sw.checked; card.classList.toggle("disabled", !ev.enabled); scheduleSaveNextConfig(); };
      rInt.oninput = () => { ev.intensity = Number(rInt.value); vInt.textContent = Math.round(ev.intensity * 100) + "%"; scheduleSaveNextConfig(); };
      rShp.oninput = () => { ev.sharpness = Number(rShp.value); vShp.textContent = sharpText(ev.sharpness); scheduleSaveNextConfig(); };
      rFrq.oninput = () => { ev.freqScale = Number(rFrq.value); vFrq.textContent = ev.freqScale.toFixed(2) + "x"; scheduleSaveNextConfig(); };

      grid.appendChild(card);
    });
    eHost.appendChild(sec);
  });

  const tHost = $("trigger-controls");
  tHost.innerHTML = "";
  [
    { key: "enableLeftBrake", label: "启用左扳机（ABS / 抱死 / 刹车推头阻力）", type: "bool" },
    { key: "enableRightAccel", label: "启用右扳机（换挡 / 烧胎 / 甩尾 / 红线）", type: "bool" },
    { key: "smartArbitration", label: "智能动态优先仲裁（根治右扳机被吞）", type: "bool" },
    { key: "brakeResistance", label: "轻踩刹车基础阻尼", type: "range", min: 0, max: 1, step: 0.05 },
    { key: "throttleMicroFeel", label: "深踩油门细腻微振", type: "range", min: 0, max: 1, step: 0.05 },
  ].forEach((item) => {
    const box = document.createElement("div");
    box.className = "ctrl-box";
    if (item.type === "bool") {
      box.innerHTML = `<div class="ctrl-head"><span>${item.label}</span><input type="checkbox" class="switch" ${c.trigger[item.key] ? "checked" : ""}></div>`;
      const chk = box.querySelector("input");
      chk.onchange = () => { c.trigger[item.key] = chk.checked; scheduleSaveNextConfig(); };
    } else {
      box.innerHTML = `<div class="ctrl-head"><span>${item.label}</span><b class="mono">${Math.round(c.trigger[item.key] * 100)}%</b></div>
        <input type="range" min="${item.min}" max="${item.max}" step="${item.step}" value="${c.trigger[item.key]}">`;
      const inp = box.querySelector("input"), out = box.querySelector("b");
      inp.oninput = () => { c.trigger[item.key] = Number(inp.value); out.textContent = Math.round(c.trigger[item.key] * 100) + "%"; scheduleSaveNextConfig(); };
    }
    tHost.appendChild(box);
  });
}

// ==================== CAR PERSONALITY & GARAGE ====================

function formatPctHalf(ratio) {
  const pct = Math.round((ratio || 0.925) * 200) / 2;
  return Number.isInteger(pct) ? `${pct}%` : `${pct.toFixed(1)}%`;
}

function updateCarCard(car, syncSliders = false) {
  if (!car) return;
  $("hud-car-label").textContent = car.customName || car.label || "通用均衡";
  $("car-archetype-cat").textContent = car.category || "动态画像";
  $("car-hero-title").textContent = (car.customName ? `${car.customName} · ` : "") + (car.label || "等待车辆");
  $("car-hero-tags").innerHTML = (car.tags || []).map((t) => `<span class="preset-tag">${esc(t)}</span>`).join("");

  $("cm-ord").textContent = car.carOrdinal ? `#${car.carOrdinal}` : "未接入";
  $("cm-cyl").textContent = car.carOrdinal ? `${car.isElectric ? "纯电" : car.cylinders + "缸"} · ${DRIVE_LABELS[car.driveTrain] || car.driveTrain}` : "—";
  $("cm-pi").textContent = car.carOrdinal ? `${CLASS_LABELS[car.carClass] || ""} ${car.carPi}` : "—";
  $("cm-susp").textContent = car.inferredMaxSuspM > 0.02 ? `${(car.inferredMaxSuspM * 100).toFixed(1)} cm` : "驾驶中反推...";
  $("cm-boost").textContent = car.peakBoostPsi > 0.5 ? `${car.peakBoostPsi.toFixed(1)} psi` : "自然吸气 / 待测";
  $("cm-pwr").textContent = car.peakPowerKw > 1 ? `${car.peakPowerKw.toFixed(0)} kW / ${car.peakTorqueNm.toFixed(0)} Nm` : "待测";

  const rlRatio = car.redlineRatio || 0.925;
  const rlPct = Math.round(rlRatio * 200) / 2;
  const rlText = formatPctHalf(rlRatio);
  $("cm-rl-val").textContent = car.isElectric ? "纯电" : rlText;
  $("hud-redline-tag").textContent = car.isElectric
    ? "纯电无红线"
    : (car.redlineLearned ? "自学习红线 " : "预估红线 ") + rlText;
  $("hud-redline-mark").style.left = Math.min(98.5, Math.max(70, rlPct)) + "%";
  $("hud-redline-mark").hidden = !!car.isElectric;
  $("cm-rl-bar").style.width = Math.round(Math.max(12, (car.redlineConfidence || 0) * 100)) + "%";
  $("cm-rl-status").textContent = car.isElectric ? "纯电车型无需红线" :
    car.redlineLearned
      ? (car.peakPowerRpm > 0 || car.learnedLimiterRpm > 0
          ? `已锁定 (峰值功率 ${Math.round(car.peakPowerRpm || 0)} rpm · 断油 ${Math.round(car.learnedLimiterRpm || 0)} rpm)`
          : `已手动锁定 (${rlText})`)
      : car.peakPowerRpm > 0 ? `学习中 (已测功率峰 ${Math.round(car.peakPowerRpm)} rpm)` : "等待全油门加速学习";

  $("car-override-badge").textContent = car.userOverride ? "当前：个人专属调校" : "当前：自动动态适配";

  if (syncSliders) {
    state.redlineEdited = false;
    $("ov-name").value = car.customName || "";
    setOvSlider("ov-round", Math.round((car.engineRoundness ?? 0.6) * 100));
    setOvSlider("ov-burble", Math.round((car.engineBurble ?? 0.3) * 100));
    setOvSlider("ov-stiff", Math.round((car.suspensionStiffness ?? 0.55) * 100));
    setOvSlider("ov-shift", Math.round((car.shiftCrispness ?? 0.72) * 100));
    setOvSlider("ov-bed", Math.round((car.engineBedScale ?? 1.0) * 100));
    $("ov-rl").value = String(rlPct);
    $("ov-rl-v").textContent = rlText;
  }

  document.querySelectorAll(".arch-card").forEach((el) => {
    el.classList.toggle("active", el.dataset.arch === car.archetypeId);
  });
}

function setOvSlider(id, val) { $(id).value = String(val); $(id + "-v").textContent = val + "%"; }
["ov-round", "ov-burble", "ov-stiff", "ov-shift", "ov-bed"].forEach((id) => {
  $(id).oninput = () => { $(id + "-v").textContent = $(id).value + "%"; };
});
$("ov-rl").oninput = () => {
  state.redlineEdited = true;
  $("ov-rl-v").textContent = formatPctHalf(Number($("ov-rl").value) / 100);
};

$("ov-save").onclick = async () => {
  try {
    const r = await api("/api/next/car", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        action: "override",
        customName: $("ov-name").value.trim(),
        engineRoundness: Number($("ov-round").value) / 100,
        engineBurble: Number($("ov-burble").value) / 100,
        suspensionStiffness: Number($("ov-stiff").value) / 100,
        shiftCrispness: Number($("ov-shift").value) / 100,
        engineBedScale: Number($("ov-bed").value) / 100,
        redlineRatio: state.redlineEdited ? Number($("ov-rl").value) / 100 : 0,
      }),
    });
    state.activeCar = r.car;
    state.garage = r.garage || [];
    updateCarCard(r.car, true);
    renderGarage();
    toast("已保存为当前车辆专属手感！");
  } catch (e) { toast(e.message, true); }
};

$("ov-reset").onclick = async () => {
  try {
    const r = await api("/api/next/car", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "reset" }),
    });
    state.activeCar = r.car;
    state.garage = r.garage || [];
    updateCarCard(r.car, true);
    renderGarage();
    toast("已恢复自动物理画像匹配");
  } catch (e) { toast(e.message, true); }
};

async function refreshGarageList() {
  if (state.exiting) return;
  try {
    const r = await api("/api/next/garage");
    if (state.exiting) return;
    state.garage = r.garage || [];
    renderGarage();
  } catch { /* ignore */ }
}

function renderArchetypes() {
  const host = $("archetype-grid");
  host.innerHTML = "";
  state.archetypes.forEach((a) => {
    const card = document.createElement("button");
    card.type = "button";
    card.className = "arch-card" + (state.activeCar?.archetypeId === a.id ? " active" : "");
    card.dataset.arch = a.id;
    card.innerHTML = `
      <div class="arch-top"><strong>${esc(a.label)}</strong><span class="preset-tag">${esc(a.category)}</span></div>
      <div class="muted">${esc(a.description)}</div>
      <div class="arch-bars">
        <span>${Math.round(a.engineFreqBaseHz || 25)}–${Math.round(a.engineFreqTopHz || 95)}Hz · 振幅 ${Math.round((a.engineBedScale || 1) * 100)}%</span>
        <span>悬架硬朗 ${Math.round(a.suspensionStiffness * 100)}%</span>
      </div>`;
    card.onclick = async () => {
      try {
        const r = await api("/api/next/car", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ action: "previewArchetype", archetypeId: a.id }),
        });
        state.activeCar = r.car;
        updateCarCard(r.car, true);
        toast(`已加载性格模板：${a.label}（正在试震引擎）`);
        triggerTest("engineRev");
      } catch (e) { toast(e.message, true); }
    };
    host.appendChild(card);
  });
}

function renderGarage() {
  $("garage-count").textContent = `${state.garage.length} 辆`;
  const host = $("garage-list");
  if (!state.garage.length) {
    host.innerHTML = `<div class="muted">暂无已记忆车辆。进入游戏驾驶任意车辆，系统会自动记录红线与物理悬挂数据。</div>`;
    return;
  }
  host.innerHTML = "";
  state.garage.forEach((g) => {
    const isEv = g.cylinders === 0;
    const rlPart = isEv ? "" : ` · 红线 ${formatPctHalf(g.redlineRatio || 0.925)}`;
    const row = document.createElement("div");
    row.className = "garage-item";
    row.innerHTML = `<div>
      <strong>${esc(g.customName || `Car #${g.carOrdinal}`)}</strong>
      <div class="muted mono">#${g.carOrdinal} · ${isEv ? "EV" : g.cylinders + "缸"} · PI ${g.carPi}${rlPart} ${g.userOverride ? "· 专属调校" : ""}</div>
    </div>
    <button class="btn sm danger">删除</button>`;
    row.querySelector("button").onclick = async () => {
      try {
        const r = await api("/api/next/garage", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ carOrdinal: g.carOrdinal }),
        });
        state.garage = r.garage || [];
        if (r.car) {
          state.activeCar = r.car;
          updateCarCard(r.car, r.car.carOrdinal === g.carOrdinal);
        }
        renderGarage();
        toast(`已移除 Car #${g.carOrdinal}`);
      } catch (e) {
        toast("删除车辆失败：" + e.message, true);
      }
    };
    host.appendChild(row);
  });
}

// ==================== PRO MODE (V4 FALLBACK — FIXED SSE + CACHED HELLO) ====================

$("pro-cat-filters").addEventListener("click", (e) => {
  const b = e.target.closest("[data-cat]");
  if (!b) return;
  state.proFilter = b.dataset.cat;
  $("pro-cat-filters").querySelectorAll("button").forEach((x) => x.classList.toggle("active", x === b));
  renderProPresets();
});

function renderProPresets() {
  const host = $("pro-preset-grid");
  host.innerHTML = "";
  const activeId = state.proStatus?.activePresetId || "";
  const list = state.proPresets.filter((p) => state.proFilter === "全部" || p.category === state.proFilter);
  list.forEach((p) => {
    const card = document.createElement("div");
    card.className = "preset-card" + (p.id === activeId ? " active" : "");
    card.innerHTML = `
      <div class="preset-head">
        <strong>${esc(p.name)}</strong>
        <span class="badge ${p.category === "竞速" ? "amber" : "cyan"}">${esc(p.category)}</span>
      </div>
      <div class="muted">${esc(p.summary)}</div>
      <div class="preset-tags">${(p.tags || []).map((t) => `<span class="preset-tag">${esc(t)}</span>`).join("")}</div>
      <div class="preset-foot">
        <span class="muted">作者：${esc(p.author)} · ${p.paramCount} 项参数</span>
        <button class="btn sm ${p.id === activeId ? "primary" : ""}">${p.id === activeId ? "使用中" : "一键应用"}</button>
      </div>`;
    card.querySelector("button").onclick = async () => {
      try {
        const r = await api("/api/pro/preset/apply", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ id: p.id }),
        });
        state.proStatus = r.status;
        renderProPresets();
        refreshProSettings();
        toast(`V4 专业模式已应用：${p.name}`);
      } catch (e) { toast("应用失败：" + e.message, true); }
    };
    host.appendChild(card);
  });
}

function applyProHello(m) {
  if (!m) return;
  state.proHello = m;
  const sel = $("pro-device-select");
  const prev = sel.value;
  sel.innerHTML = "";
  const devs = m.devices || [];
  if (!devs.length) {
    sel.innerHTML = `<option value="-1">V4 未发现 33B 手柄（请点「刷新手柄」）</option>`;
    $("pro-dev-start").disabled = true;
    $("pro-dev-stop").disabled = !m.writing;
  } else {
    devs.forEach((d) => {
      const opt = document.createElement("option");
      opt.value = String(d.id);
      opt.textContent = `${d.product || "手柄"} (${d.vid}:${d.pid} · 0x${d.usagePage})${d.writing ? " [V4 输出中]" : ""}`;
      if (d.writing) opt.selected = true;
      sel.appendChild(opt);
    });
    if (!m.writing && devs.some((d) => String(d.id) === prev)) sel.value = prev;
    $("pro-dev-start").disabled = !!m.writing;
    $("pro-dev-stop").disabled = !m.writing;
  }
  $("pro-dev-msg").textContent = m.deviceMessage || "";
  if (state.mode === "pro") {
    setPill("pill-hid", m.writing ? "ok" : "", m.writing ? "V4 输出中" : "V4 未开启");
  }
}

function updateProDiagnosticsLink() {
  const link = $("pro-diag-link");
  if (!link) return;
  if (state.proStatus?.httpBase) {
    link.href = `${state.proStatus.httpBase}/diagnostics.html`;
  } else {
    link.href = "/v4/diagnostics.html";
  }
}

function applyProLiveStatus(m) {
  if (!m) return;
  state.proLiveStatus = m;
  const writing = !!(m.writing ?? state.proHello?.writing);
  $("pro-dev-stats").textContent = `V4 已发 ${(m.written || 0).toLocaleString()} 帧${m.writeFailures ? ` · 异常 ${m.writeFailures}` : ""}`;
  if (state.mode === "pro") {
    setPill("pill-hid", writing ? (m.writeFailures > 0 ? "warn" : "ok") : "", writing ? "V4 输出中" : "V4 未开启");
  }
}

async function fetchProHelloFallback() {
  if (state.mode !== "pro") return;
  try {
    const r = await api("/api/pro/hello");
    if (r.hello) applyProHello(r.hello);
    if (r.live) applyProLiveStatus(r.live);
  } catch { /* ignore */ }
}

async function refreshProSettings() {
  try {
    const st = await api("/api/pro/settings");
    state.proSettings = st;
    const raw = st.config?.settings || st.settings || {};
    $("pro-active-summary").textContent = `配置：${st.profile || st.config?.profile || "自定义"} · Revision #${st.revision ?? "-"} · 共 ${Object.keys(raw).length} 项`;
    const showKeys = [
      ["enable_traction_feedback", "三段抓地反馈"],
      ["grip_road_mode", "握把路感分工 (2=左右)"],
      ["grip_rough_light_threshold", "轻非铺装阈值"],
      ["grip_rough_heavy_threshold", "重非铺装阈值"],
      ["enable_grip_rumble_strip", "路肩反馈"],
      ["grip_engine_gain", "引擎总增益"],
      ["grip_shift_gain", "换挡握把增益"],
      ["grip_wall_impact_min_speed_kmh", "撞墙最低速 km/h"],
      ["grip_landing_suspension_threshold", "落地悬挂阈值"],
      ["redline_ratio", "红线触发比例"],
      ["brake_baseline_force", "刹车基础阻力"],
      ["grip_master_gain", "握把总倍率"],
    ];
    $("pro-kv-grid").innerHTML = showKeys.map(([k, lbl]) =>
      `<div class="pro-kv"><span>${lbl}</span><b class="mono">${raw[k] !== undefined ? esc(raw[k]) : "—"}</b></div>`
    ).join("");
  } catch { /* V4 starting */ }
}

$("pro-import-btn").onclick = () => $("pro-import-file").click();
$("pro-import-file").onchange = async (e) => {
  const f = e.target.files[0];
  if (!f) return;
  try {
    const txt = await f.text();
    const r = await api("/api/pro/import", { method: "POST", headers: { "Content-Type": "application/json" }, body: txt });
    state.proStatus = r.status;
    renderProPresets();
    refreshProSettings();
    toast("已导入自定义 V4 配置文件");
  } catch (err) { toast(err.message, true); }
  e.target.value = "";
};

$("pro-export-btn").onclick = async () => {
  try {
    const st = await api("/api/pro/settings");
    const cfg = st.config || st;
    const blob = new Blob([JSON.stringify(cfg, null, 2)], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `V4_${cfg.profile || "custom"}.json`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  } catch (e) { toast(e.message, true); }
};

function clearProRecTimer() {
  if (state.proRecTimer) {
    clearTimeout(state.proRecTimer);
    state.proRecTimer = 0;
  }
}

function connectProStream() {
  disconnectProStream();
  if (state.exiting || state.mode !== "pro") return;
  const es = new EventSource("/api/pro/events");
  state.proSSE = es;
  es.onmessage = (e) => {
    if (state.exiting || state.mode !== "pro") return;
    let m; try { m = JSON.parse(e.data); } catch { return; }
    if (m.type === "hello") applyProHello(m);
    if (m.type === "status") applyProLiveStatus(m);
  };
  es.onerror = () => {
    if (state.exiting || state.mode !== "pro") {
      disconnectProStream();
      return;
    }
    if (es.readyState === EventSource.CLOSED) {
      if (state.proSSE === es) state.proSSE = null;
      setPill("pill-hid", "warn", "V4 重连中");
      if (!state.proSSERetryTimer) {
        state.proSSERetryTimer = setTimeout(() => {
          state.proSSERetryTimer = 0;
          if (!state.exiting && state.mode === "pro") {
            connectProStream();
            fetchProHelloFallback();
          }
        }, 1500);
      }
    }
  };
  pollProRecording();
}

function disconnectProStream() {
  clearProRecTimer();
  if (state.proSSERetryTimer) {
    clearTimeout(state.proSSERetryTimer);
    state.proSSERetryTimer = 0;
  }
  if (state.proSSE) {
    state.proSSE.close();
    state.proSSE = null;
  }
}

async function pollProRecording() {
  clearProRecTimer();
  if (state.exiting || state.mode !== "pro") return;
  try {
    const r = await api("/api/pro/recording");
    if (state.exiting || state.mode !== "pro") return;
    state.proRec = r;
    const exporting = !!r.exporting;
    $("pro-rec-btn").disabled = exporting;
    $("pro-rec-btn").textContent = exporting
      ? "导出中..."
      : r.recording
        ? `停止录制 (${r.packets}包)`
        : "开始录制";
    $("pro-rec-dl").hidden = !(!r.recording && !exporting && r.downloadUrl);
  } catch {
    if (state.exiting || state.mode !== "pro") return;
  }
  if (!state.exiting && state.mode === "pro") {
    state.proRecTimer = setTimeout(pollProRecording, 1500);
  }
}

$("pro-dev-refresh").onclick = async () => {
  try {
    await api("/api/pro/device", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ refresh: true }) });
    setTimeout(fetchProHelloFallback, 200);
    toast("已刷新 V4 手柄列表");
  } catch (e) { toast(e.message, true); }
};

$("pro-dev-start").onclick = async () => {
  const id = Number($("pro-device-select").value);
  if (id < 0) return toast("请先选择有效手柄", true);
  try {
    await api("/api/pro/device", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ gen: state.proHello?.deviceGeneration || 0, id }),
    });
    setTimeout(fetchProHelloFallback, 200);
    toast("已向 V4 发送开启反馈指令");
  } catch (e) { toast("V4 开启失败：" + e.message, true); }
};

$("pro-dev-stop").onclick = async () => {
  try {
    await api("/api/pro/device", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ gen: state.proHello?.deviceGeneration || 0, id: -1 }),
    });
    setTimeout(fetchProHelloFallback, 200);
    toast("V4 手柄反馈已停止");
  } catch (e) { toast(e.message, true); }
};

$("pro-rec-btn").onclick = async () => {
  if (state.proRec?.exporting) return;
  const action = state.proRec?.recording ? "stop" : "start";
  $("pro-rec-btn").disabled = true;
  try {
    await api("/api/pro/recording", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ action }) });
    toast(action === "start" ? "已开始 V4 诊断录制" : "正在打包导出诊断包...");
  } catch (e) {
    toast(e.message, true);
  }
  pollProRecording();
};

// ==================== LIVE SSE STREAM & VISUALIZERS ====================

let lastCarOrdinalSeen = 0;
let mainSseOffline = false;

function connectMainSSE() {
  if (state.mainSSE) {
    state.mainSSE.close();
    state.mainSSE = null;
  }
  if (state.exiting) return;
  const es = new EventSource("/api/stream");
  state.mainSSE = es;
  es.onopen = () => {
    if (mainSseOffline && !state.exiting) {
      mainSseOffline = false;
      loadInitialState();
    }
  };
  es.onerror = () => {
    if (state.exiting) {
      es.close();
      if (state.mainSSE === es) state.mainSSE = null;
      return;
    }
    mainSseOffline = true;
    setPill("pill-game", "bad", "服务已断开");
    setPill("pill-hid", "bad", "连接中断");
  };
  es.onmessage = (e) => {
    if (state.exiting) return;
    let m; try { m = JSON.parse(e.data); } catch { return; }
    state.latestSnap = m.snap;

    if (m.mode && m.mode !== state.mode && !state.switchingMode) {
      state.mode = m.mode;
      renderModeUI();
      if (m.mode !== "pro") {
        disconnectProStream();
        state.proHello = null;
      }
      loadInitialState();
    }

    if (m.proStatus) {
      const prevRunning = state.proStatus?.running;
      const prevPreset = state.proStatus?.activePresetId;
      state.proStatus = m.proStatus;
      updateProDiagnosticsLink();
      if (prevPreset !== m.proStatus.activePresetId) {
        renderProPresets();
      }
      if (state.mode === "pro" && prevRunning && !m.proStatus.running) {
        setPill("pill-hid", "bad", "V4 已停止");
        if (m.proStatus.lastError) {
          toast("V4 专业引擎已停止：" + m.proStatus.lastError, true);
        }
      }
    }

    if (m.mode === "entertainment") {
      const newWriting = !!m.writing;
      if (newWriting !== state.writing) {
        state.writing = newWriting;
        renderEntDevices();
      }
      setPill("pill-hid", newWriting ? (m.writeFailures > 0 ? "warn" : "ok") : "", newWriting ? "Next 输出中" : "反馈关闭");
      $("ent-dev-stats").textContent = `已发 ${(m.written || 0).toLocaleString()} 帧${m.writeFailures ? ` · 异常 ${m.writeFailures}` : ""}`;
    }
    const gameOk = !!m.snap?.gameConnected;
    setPill("pill-game", gameOk ? "ok" : "", gameOk ? `游戏运行中 (${Math.round(m.snap.packet?.speedKmh || 0)} km/h)` : "等待游戏遥测");

    if (m.snap) {
      pushRing(m.snap.gripL, m.snap.gripR, m.snap.trig);
      updateDashboard(m.snap);
      if (m.snap.car) {
        state.activeCar = m.snap.car;
        const changed = m.snap.car.carOrdinal !== lastCarOrdinalSeen;
        if (changed) {
          lastCarOrdinalSeen = m.snap.car.carOrdinal;
          if (m.snap.car.carOrdinal > 0) {
            refreshGarageList();
          }
        }
        updateCarCard(m.snap.car, changed);
      }
    }
    if (m.lighting) {
      state.lighting = m.lighting;
      updateLightingUI(m.lighting);
    }
  };
}

function pushRing(gl, gr, tr) {
  if (!gl || !gr || !tr) return;
  for (let i = 0; i < 8; i++) {
    state.ringGL[state.ringPos] = gl[i] ?? 128;
    state.ringGR[state.ringPos] = gr[i] ?? 128;
    state.ringT[state.ringPos] = tr[i] ?? 128;
    state.ringPos = (state.ringPos + 1) % state.ringN;
  }
}

function updateDashboard(s) {
  const p = s.packet || {};
  $("hud-speed").textContent = Math.round(p.speedKmh || 0);
  $("hud-gear").textContent = !p.active ? "—" : p.gear === 0 ? "R" : String(p.gear);
  $("hud-rpm").textContent = Math.round(p.rpm || 0);
  $("hud-maxrpm").textContent = Math.round(p.maxRpm || 8000);
  $("hud-rpm-fill").style.width = Math.min(100, Math.max(0, (p.rpmRatio || 0) * 100)) + "%";
  $("hud-surface").textContent = s.surfaceLabel || "静止";

  const accPct = Math.round((p.accel || 0) / 255 * 100);
  const brkPct = Math.round((p.brake || 0) / 255 * 100);
  $("bar-accel").style.width = accPct + "%";
  $("val-accel").textContent = accPct + "%";
  $("bar-brake").style.width = brkPct + "%";
  $("val-brake").textContent = brkPct + "%";
  $("hud-power").textContent = `${((p.power || 0) / 1000).toFixed(0)} kW · ${(p.boost || 0).toFixed(1)} psi`;
  $("hud-gx").textContent = ((p.accelX || 0) / 9.81).toFixed(2) + " g";
  $("hud-gz").textContent = ((p.accelZ || 0) / 9.81).toFixed(2) + " g";
  $("hud-gy").textContent = ((p.accelY || 0) / 9.81).toFixed(2) + " g";

  if (s.impactActive) {
    state.lastImpactAt = performance.now();
    state.lastImpactAngle = s.impactAngleDeg || 0;
    state.lastImpactLabel = s.impactLabel || "撞击";
  }
  const impFresh = performance.now() - state.lastImpactAt < 1800;
  $("hud-impact").textContent = impFresh ? `${state.lastImpactLabel} (${Math.round(state.lastImpactAngle)}°)` : "无";

  const ns = p.normalizedSuspensionTravel || [0, 0, 0, 0];
  const sr = p.surfaceRumble || [0, 0, 0, 0];
  const pd = p.puddleDepth || [0, 0, 0, 0];
  const sl = p.combinedSlip || [0, 0, 0, 0];
  for (let i = 0; i < 4; i++) {
    const pct = Math.min(100, Math.max(0, Math.round((ns[i] || 0) * 100)));
    $(`wh-${i}-susp`).textContent = pct + "%";
    $(`wh-${i}-bar`).style.width = pct + "%";
    $(`wh-${i}-slip`).textContent = `滑移 ${(Math.abs(sl[i] || 0)).toFixed(2)}`;
    const card = $(`wh-${i}`);
    const isWater = (pd[i] || 0) >= 0.03;
    const isHot = Math.abs(sl[i] || 0) > 0.75 || (sr[i] || 0) >= 0.07;
    card.classList.toggle("water", isWater);
    card.classList.toggle("hot", !isWater && isHot);
    $(`wh-${i}-surf`).textContent = isWater ? `水深 ${(pd[i] * 100).toFixed(0)}%` :
      (sr[i] || 0) >= 0.22 ? "重砂石" : (sr[i] || 0) >= 0.07 ? "土路" : "柏油";
  }

  const evs = s.events || [];
  const activeSet = new Set(evs.map((x) => (x.id === "smashable" ? "collisionWall" : x.id)));
  $("live-event-chips").innerHTML = evs.length
    ? evs.map((x) => `<span class="chip cat-${esc(x.category)}">${esc(x.label)} ${Math.round((x.intensity || 0) * 100)}%</span>`).join("")
    : `<span class="chip muted">当前平稳 / 点击下方对比试震体验不同手感</span>`;

  document.querySelectorAll(".ev-card").forEach((c) => {
    c.classList.toggle("firing", activeSet.has(c.dataset.evKey));
  });

  $("scope-meta").textContent = `削波 ${s.clippedFrames || 0} · 智能仲裁 ${s.conflictsFixed || 0}`;
  $("rms-gl").textContent = `RMS ${(s.gripLRms || 0).toFixed(1)}`;
  $("rms-gr").textContent = `RMS ${(s.gripRRms || 0).toFixed(1)}`;
  $("flags-readout").textContent = `flags 0x${s.flagsHex || "84"}`;
  $("scope-trig-lbl").textContent = s.trigRoute === 0 ? "扳机 T（左 · 制动/推头优先）" : s.trigRoute === 1 ? "扳机 T（右 · 动力/甩尾优先）" : s.trigRoute === 2 ? "扳机 T（双扳机同步 · 四轮漂移）" : "扳机 T（中性）";
  if (s.lastReportHex) {
    $("wire-hex").textContent = s.lastReportHex.match(/.{1,2}/g).join(" ");
  }
}

function drawVisuals() {
  if (state.exiting) {
    state.rafId = 0;
    return;
  }
  if (!document.hidden && state.mode === "entertainment") {
    drawScope("scope-gl", state.ringGL, "#38bdf8");
    drawScope("scope-gr", state.ringGR, "#a78bfa");
    drawScope("scope-t", state.ringT, "#f59e0b");
    drawGCircleAndImpact(state.latestSnap?.packet);
  }
  state.rafId = requestAnimationFrame(drawVisuals);
}

function drawScope(id, ring, color) {
  const cv = $(id);
  if (!cv) return;
  const w = cv.clientWidth;
  if (!w) return;
  const h = 58, dpr = window.devicePixelRatio || 1;
  if (cv.width !== Math.round(w * dpr) || cv.height !== Math.round(h * dpr)) {
    cv.width = Math.round(w * dpr);
    cv.height = Math.round(h * dpr);
  }
  const g = cv.getContext("2d");
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);

  g.strokeStyle = "rgba(148,163,184,0.22)";
  g.setLineDash([4, 4]);
  g.beginPath(); g.moveTo(0, h / 2); g.lineTo(w, h / 2); g.stroke();
  g.setLineDash([]);

  g.strokeStyle = color;
  g.lineWidth = 1.7;
  g.beginPath();
  const n = state.ringN;
  for (let i = 0; i < n; i++) {
    const val = ring[(state.ringPos + i) % n];
    const x = (i / (n - 1)) * w;
    const y = h - 3 - (val / 255) * (h - 6);
    if (i === 0) g.moveTo(x, y); else g.lineTo(x, y);
  }
  g.stroke();
}

function drawGCircleAndImpact(p) {
  const cv = $("g-canvas");
  if (!cv || !cv.clientWidth) return;
  const g = cv.getContext("2d"), W = cv.width, cx = W / 2, R = W / 2 - 12;
  g.clearRect(0, 0, W, W);

  g.strokeStyle = "rgba(148,163,184,0.22)";
  g.lineWidth = 1;
  [R, R * 0.5].forEach((r) => { g.beginPath(); g.arc(cx, cx, r, 0, Math.PI * 2); g.stroke(); });
  g.beginPath(); g.moveTo(cx - R, cx); g.lineTo(cx + R, cx); g.moveTo(cx, cx - R); g.lineTo(cx, cx + R); g.stroke();

  // Draw glowing 360° directional impact arc if recent hit!
  // Impact angle convention: 0=Front(top), +90=Left hit(left), -90=Right hit(right), +-180=Rear(bottom)
  const age = performance.now() - state.lastImpactAt;
  if (age < 1200) {
    const alpha = Math.max(0, 1 - age / 1200);
    // Canvas angle: top is -PI/2, left is PI, right is 0, bottom is +PI/2
    const rad = -Math.PI / 2 - (state.lastImpactAngle * Math.PI) / 180;
    g.strokeStyle = `rgba(251,113,133,${alpha.toFixed(2)})`;
    g.lineWidth = 7;
    g.beginPath();
    g.arc(cx, cx, R + 2, rad - 0.45, rad + 0.45);
    g.stroke();
  }

  // Tiny top-down car silhouette
  g.fillStyle = "rgba(148,163,184,0.22)";
  g.fillRect(cx - 7, cx - 13, 14, 26);

  // Live G-force dot
  const gx = Math.max(-2, Math.min(2, (p?.accelX || 0) / 9.81));
  const gz = Math.max(-2, Math.min(2, (p?.accelZ || 0) / 9.81));
  const px = cx + (gx / 2) * R;
  const py = cx - (gz / 2) * R;

  g.strokeStyle = "rgba(56,189,248,0.5)";
  g.lineWidth = 1.5;
  g.beginPath(); g.moveTo(cx, cx); g.lineTo(px, py); g.stroke();
  g.fillStyle = "#38bdf8";
  g.beginPath(); g.arc(px, py, 5.5, 0, Math.PI * 2); g.fill();
}

// ==================== K6 / APEX 6 TELEMETRY LIGHTING ====================

const ledLampEls = (() => {
  const host = $("led-lamps");
  if (!host) return [];
  host.innerHTML = "";
  return Array.from({ length: 12 }, (_, i) => {
    const el = document.createElement("span");
    el.className = "led-lamp";
    el.style.setProperty("--lamp", i < 6 ? "#20e58b" : i < 9 ? "#ffd13b" : "#ff2a4d");
    host.appendChild(el);
    return el;
  });
})();

let ledSaveSeq = 0;

function syncLightingControls(cfg) {
  if (!cfg || !$("led-enabled")) return;
  $("led-enabled").checked = !!cfg.enabled;
  $("led-opt-rpm").checked = !!cfg.enableRpm;
  $("led-opt-shift").checked = !!cfg.enableShiftFlash;
  $("led-opt-brake").checked = !!cfg.enableHardBrake;
  $("led-opt-abs").checked = !!cfg.enableAbs;
  $("led-opt-idle").checked = !!cfg.autoRestoreOnIdle;
  $("led-opt-vib").checked = !!cfg.vibrationLink;
  const brkPct = Math.round((cfg.hardBrakeThreshold ?? 0.35) * 100);
  $("led-brk-th").value = String(brkPct);
  $("led-brk-th-v").textContent = brkPct + "%";
  const brPct = Math.round((cfg.brightnessScale ?? 1.0) * 100);
  $("led-bright").value = String(brPct);
  $("led-bright-v").textContent = brPct + "%";
  const isAuto = !(cfg.shiftRatioOverride >= 0.72);
  $("led-shift-auto").checked = isAuto;
  $("led-shift").disabled = isAuto;
  if (!isAuto) {
    const shPct = Math.min(98, Math.max(72, Math.round(cfg.shiftRatioOverride * 100)));
    $("led-shift").value = String(shPct);
    $("led-shift-v").textContent = shPct + "%";
  } else {
    $("led-shift-v").textContent = "自动";
  }
}

function collectLightingConfig() {
  const isAuto = $("led-shift-auto").checked;
  const shVal = Number($("led-shift").value);
  return {
    enabled: $("led-enabled").checked,
    enableRpm: $("led-opt-rpm").checked,
    enableShiftFlash: $("led-opt-shift").checked,
    enableHardBrake: $("led-opt-brake").checked,
    enableAbs: $("led-opt-abs").checked,
    autoRestoreOnIdle: $("led-opt-idle").checked,
    vibrationLink: $("led-opt-vib").checked,
    hardBrakeThreshold: Number($("led-brk-th").value) / 100,
    brightnessScale: Number($("led-bright").value) / 100,
    shiftRatioOverride: isAuto ? 0 : Math.min(98, Math.max(72, shVal)) / 100,
  };
}

function scheduleSaveLightingConfig(immediate = false) {
  clearTimeout(state.ledSaveTimer);
  state.ledSaveTimer = 0;
  const seq = ++ledSaveSeq;
  const run = async () => {
    state.ledSaveTimer = 0;
    try {
      const cfg = collectLightingConfig();
      const r = await api("/api/lighting/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(cfg),
      });
      if (r.lighting) {
        state.lighting = r.lighting;
        if (seq === ledSaveSeq && r.lighting.config) {
          syncLightingControls(r.lighting.config);
        }
        updateLightingUI(r.lighting);
      }
    } catch (e) {
      toast("保存灯效设置失败：" + e.message, true);
    }
  };
  if (immediate) return run();
  state.ledSaveTimer = setTimeout(run, 120);
  return Promise.resolve();
}

function updateLightingUI(ls) {
  if (!ls || !$("led-swatch")) return;
  const rgb = `rgb(${ls.r ?? 0}, ${ls.g ?? 0}, ${ls.b ?? 0})`;
  const isLit = (ls.r || 0) + (ls.g || 0) + (ls.b || 0) > 0;
  $("led-swatch").style.background = isLit ? rgb : "#1e293b";
  $("led-swatch").style.boxShadow = isLit ? `0 0 14px ${rgb}` : "none";

  const badge = $("led-cue-badge");
  badge.textContent = ls.label || "待机";
  badge.className = "badge " + (ls.cue === "brake" || ls.cue === "shift-up" ? "amber" : "cyan");

  const hwTxt = !ls.config?.enabled
    ? "灯带同步已暂停（已恢复原灯效）"
    : ls.connected && ls.backupReady
      ? `${ls.product || "APEX 6"} 已备份原灯(${ls.backupBytes}B) · 写入 ${ls.writesOk || 0}${ls.writeErrs ? ` · 异常 ${ls.writeErrs}` : ""}`
      : ls.lastError || "等待连接 APEX 6 / K6 灯效接口 (0xFFA0)...";
  $("led-hw-status").textContent = hwTxt;

  setPill("pill-led",
    !ls.config?.enabled ? "" : ls.connected && ls.backupReady ? (ls.lastError ? "warn" : "ok") : "",
    !ls.config?.enabled ? "灯带关闭" : ls.connected && ls.backupReady ? `灯带·${ls.label || "就绪"}` : "灯带待机",
  );

  const lit = ls.litLamps || 0;
  ledLampEls.forEach((el, i) => {
    const normalColor = i < 6 ? "#20e58b" : i < 9 ? "#ffd13b" : "#ff2a4d";
    el.style.setProperty("--lamp", ls.alert ? rgb : normalColor);
    el.classList.toggle("on", i < lit);
  });
}

if ($("led-cfg-toggle")) {
  $("led-cfg-toggle").onclick = () => {
    const panel = $("led-settings");
    panel.hidden = !panel.hidden;
    $("led-cfg-toggle").textContent = panel.hidden ? "设置 ▾" : "收起 ▴";
  };
  ["led-enabled", "led-opt-rpm", "led-opt-shift", "led-opt-brake", "led-opt-abs", "led-opt-idle", "led-opt-vib"].forEach((id) => {
    $(id).onchange = () => scheduleSaveLightingConfig(id === "led-enabled");
  });
  $("led-brk-th").oninput = () => {
    $("led-brk-th-v").textContent = $("led-brk-th").value + "%";
    scheduleSaveLightingConfig(false);
  };
  $("led-bright").oninput = () => {
    $("led-bright-v").textContent = $("led-bright").value + "%";
    scheduleSaveLightingConfig(false);
  };
  $("led-shift-auto").onchange = () => {
    const isAuto = $("led-shift-auto").checked;
    $("led-shift").disabled = isAuto;
    $("led-shift-v").textContent = isAuto ? "自动" : $("led-shift").value + "%";
    scheduleSaveLightingConfig(true);
  };
  $("led-shift").oninput = () => {
    $("led-shift-auto").checked = false;
    $("led-shift").disabled = false;
    $("led-shift-v").textContent = $("led-shift").value + "%";
    scheduleSaveLightingConfig(false);
  };
  $("led-restore-btn").onclick = async () => {
    try {
      const r = await api("/api/lighting/restore", { method: "POST" });
      if (r.lighting) {
        state.lighting = r.lighting;
        updateLightingUI(r.lighting);
      }
      toast("已将手柄灯带恢复为连接前的原始灯效");
    } catch (e) { toast("恢复原灯效失败：" + e.message, true); }
  };
  document.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-led-test]");
    if (!b) return;
    try {
      if (!$("led-enabled").checked) {
        $("led-enabled").checked = true;
        await scheduleSaveLightingConfig(true);
      }
      await api("/api/lighting/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ mode: b.dataset.ledTest }),
      });
    } catch (err) { toast("灯效测试失败：" + err.message, true); }
  });
}

if ($("btn-safe-exit")) {
  $("btn-safe-exit").onclick = async () => {
    $("btn-safe-exit").disabled = true;
    $("btn-safe-exit").textContent = "退出中...";
    toast("正在恢复手柄原灯效并安全关闭引擎...");
    try {
      await api("/api/shutdown", { method: "POST" });
      state.exiting = true;
      clearTimeout(state.saveTimer);
      state.saveTimer = 0;
      clearTimeout(state.ledSaveTimer);
      state.ledSaveTimer = 0;
      disconnectProStream();
      if (state.mainSSE) {
        state.mainSSE.close();
        state.mainSSE = null;
      }
      if (state.rafId) {
        cancelAnimationFrame(state.rafId);
        state.rafId = 0;
      }
      $("alert-bar").hidden = false;
      $("alert-bar").textContent = "✓ ForzaNext 已恢复手柄原灯效并安全退出，您现在可以关闭此浏览器标签页。";
      setPill("pill-hid", "", "已退出");
      setPill("pill-led", "", "原灯效已恢复");
      setPill("pill-udp", "", "已关闭");
      setPill("pill-game", "", "已退出");
      toast("已安全退出，可关闭页面");
    } catch (e) {
      toast("安全退出请求失败：" + e.message, true);
      $("btn-safe-exit").disabled = false;
      $("btn-safe-exit").textContent = "安全退出";
    }
  };
}

loadInitialState();
connectMainSSE();
state.rafId = requestAnimationFrame(drawVisuals);
