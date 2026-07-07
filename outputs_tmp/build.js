const pptxgen = require("pptxgenjs");
const pres = new pptxgen();
pres.layout = "LAYOUT_WIDE"; // 13.3 x 7.5
pres.author = "WTG";
pres.title = "WTG 프로세스 간 통신 전체 그림";

// ---- palette ----
const DARK = "1B2432", TEAL = "0E7C86", MUTED = "64748B";
const NODEFILL = "F4F6F8", NODEBORD = "C3CCD6", WHITE = "FFFFFF";
// transport colors
const T = {
  udp:  "E8833A", // UDP FIX
  mymq: "1E3A8A", // MyMQ binary wire
  grpc: "0E7C86", // gRPC / HTTP2
  http: "3B82C4", // HTTP / REST
  ws:   "8B5C9E", // WebSocket
  fix:  "9A7B4F", // FIX 4.4 / TCP
  pg:   "2E8B57", // PostgreSQL pgx
  redis:"C0392B", // Redis RESP
  shm:  "D81B7A", // SHM (proposed)
};

const shadow = () => ({ type: "outer", color: "000000", blur: 5, offset: 2, angle: 90, opacity: 0.12 });

function box(s, x, y, w, h, title, sub, opt) {
  opt = opt || {};
  s.addShape(pres.shapes.ROUNDED_RECTANGLE, {
    x, y, w, h, rectRadius: 0.06,
    fill: { color: opt.fill || NODEFILL }, line: { color: opt.bord || NODEBORD, width: 1 },
    shadow: shadow(),
  });
  const runs = [{ text: title, options: { bold: true, fontSize: opt.ts || 9.5, color: opt.tc || DARK, breakLine: !!sub } }];
  if (sub) runs.push({ text: sub, options: { fontSize: 7, color: opt.sc || MUTED } });
  s.addText(runs, { x, y, w, h, align: "center", valign: "middle", fontFace: "Calibri", margin: 2 });
}

function cx(x, w) { return x + w / 2; }
// connector from point (x1,y1) to (x2,y2)
function conn(s, x1, y1, x2, y2, color, dash) {
  const x = Math.min(x1, x2), y = Math.min(y1, y2);
  const w = Math.abs(x2 - x1), h = Math.abs(y2 - y1);
  s.addShape(pres.shapes.LINE, {
    x, y, w, h,
    flipH: (x2 < x1), flipV: (y2 < y1),
    line: { color, width: 1.75, dashType: dash || "solid", endArrowType: "triangle" },
  });
}

// =========================================================
// SLIDE 1 — Title
// =========================================================
let s1 = pres.addSlide();
s1.background = { color: DARK };
s1.addShape(pres.shapes.OVAL, { x: 11.0, y: -1.4, w: 3.6, h: 3.6, fill: { color: TEAL, transparency: 82 }, line: { type: "none" } });
s1.addShape(pres.shapes.OVAL, { x: -1.1, y: 5.2, w: 3.2, h: 3.2, fill: { color: TEAL, transparency: 88 }, line: { type: "none" } });
s1.addText("WTG · 아키텍처 브리핑", { x: 0.9, y: 1.5, w: 10, h: 0.5, fontSize: 15, color: TEAL, bold: true, fontFace: "Calibri", charSpacing: 2 });
s1.addText("프로세스 간 통신 전체 그림", { x: 0.85, y: 2.05, w: 11.5, h: 1.0, fontSize: 42, color: WHITE, bold: true, fontFace: "Cambria" });
s1.addText("어떤 구간을 어떤 기술로 잇는가 — 경계별 전송 프로토콜 지도",
  { x: 0.9, y: 3.15, w: 11, h: 0.5, fontSize: 17, color: "CBD5E0", fontFace: "Calibri" });
s1.addText([
  { text: "UDP · MyMQ wire · gRPC · HTTP/REST · WebSocket · FIX 4.4 · PostgreSQL · Redis · SHM", options: { fontSize: 12, color: "9AA7B5" } },
], { x: 0.9, y: 4.35, w: 11.5, h: 0.4, fontFace: "Calibri" });

// =========================================================
// SLIDE 2 — Architecture diagram
// =========================================================
let s2 = pres.addSlide();
s2.background = { color: WHITE };
s2.addText("전체 통신 지도 — 노드는 프로세스, 선 색은 전송 기술",
  { x: 0.3, y: 0.18, w: 12.7, h: 0.5, fontSize: 20, bold: true, color: DARK, fontFace: "Cambria" });

// column x / row y
const A = 0.3, B = 2.75, Cc = 5.15, D = 7.75, NW = 2.0, NH = 0.5;
const R = [0.95, 1.55, 2.15, 2.75, 3.35, 3.95];
const rc = (i) => R[i] + NH / 2; // row center y

// --- draw connectors first (under boxes) ---
// ingress
conn(s2, A + NW, rc(0), Cc, rc(0), T.udp);                 // feed -> forwarder (UDP)
conn(s2, A + NW, rc(1), B, rc(1), T.http);                 // web -> edge-api
conn(s2, A + NW, rc(2), B, rc(2), T.ws);                   // sise WS -> edge-price
conn(s2, A + NW, rc(3), B, rc(3), T.ws);                   // push WS -> edge-push
conn(s2, A + NW, rc(4), B, rc(4), T.ws);                   // chart WS -> edge-chart
conn(s2, A + NW, rc(5), B, rc(5), T.fix);                  // FIX CP -> edge-fix
// edge -> internal
conn(s2, B + NW, rc(1), Cc, rc(1), T.http);               // edge-api -> mci-api
conn(s2, B + NW, rc(2), Cc, rc(2), T.grpc);               // edge-price -> mci-price
conn(s2, B + NW, rc(3), Cc, rc(3), T.grpc);               // edge-push -> mci-push
conn(s2, B + NW, rc(4), Cc, rc(4), T.http);               // edge-chart -> mci-chart
conn(s2, B + NW, rc(5), Cc, rc(1), T.http);               // edge-fix -> mci-api (/v1/tx)
// internal -> broker (MyMQ)
const bkX = D, bkY = 1.0, bkH = 1.55; const bkMidY = bkY + bkH / 2;
conn(s2, Cc + NW, rc(0), bkX, bkMidY, T.mymq);            // forwarder -> broker
conn(s2, Cc + NW, rc(1), bkX, bkMidY, T.mymq);            // api -> broker
conn(s2, Cc + NW, rc(2), bkX, bkMidY, T.mymq);            // price -> broker
conn(s2, Cc + NW, rc(3), bkX, bkMidY, T.mymq);            // push -> broker
conn(s2, Cc + NW, rc(5), bkX, bkMidY, T.mymq);            // admin -> broker
// internal -> stores
conn(s2, Cc + NW, rc(5), D, rc(3) + 0.02, T.grpc);       // admin -> etcd (write, gRPC)
conn(s2, Cc + NW, rc(2), D, rc(3), T.grpc);              // price -> etcd (watch)
conn(s2, Cc + NW, rc(1), D, rc(4), T.redis);             // api -> Redis
conn(s2, Cc + NW, rc(2), D, rc(5), T.pg);                // price -> TimescaleDB
conn(s2, Cc + NW, rc(4), D, rc(5), T.pg);                // chart -> TimescaleDB
// price -> chart (gRPC SubscribeBar), routed on left edge of column C
conn(s2, Cc + 0.05, rc(2), Cc + 0.05, rc(4), T.grpc);

// --- nodes: External (A) ---
box(s2, A, R[0], NW, NH, "시장 피드", "SMB/KMB/EBS/Reuters");
box(s2, A, R[1], NW, NH, "웹 / REST 클라이언트");
box(s2, A, R[2], NW, NH, "시세 WS 클라이언트");
box(s2, A, R[3], NW, NH, "푸시 WS 클라이언트");
box(s2, A, R[4], NW, NH, "챠트 WS 클라이언트");
box(s2, A, R[5], NW, NH, "외부 FIX 카운터파티");
// Edge (B)
box(s2, B, R[1], NW, NH, "mci-edge-api", ":8090");
box(s2, B, R[2], NW, NH, "mci-edge-price", ":8083");
box(s2, B, R[3], NW, NH, "mci-edge-push", ":8084");
box(s2, B, R[4], NW, NH, "mci-edge-chart", ":8087");
box(s2, B, R[5], NW, NH, "mci-edge-fix", ":5001");
// Internal (C)
box(s2, Cc, R[0], NW, NH, "quote-forwarder", "UDP→broker");
box(s2, Cc, R[1], NW, NH, "mci-api", ":8080");
box(s2, Cc, R[2], NW, NH, "mci-price", ":8082 / :50051");
box(s2, Cc, R[3], NW, NH, "mci-push", ":8081");
box(s2, Cc, R[4], NW, NH, "mci-chart", ":8086");
box(s2, Cc, R[5], NW, NH, "mci-admin", ":9090");
// Broker + stores (D)
box(s2, D, bkY, NW, bkH, "mymqd (broker)", ":11217 · MyMQ wire", { fill: DARK, tc: WHITE, sc: "AAB7C4", bord: DARK, ts: 11 });
box(s2, D, R[3], NW, NH, "etcd", "카탈로그/정책");
box(s2, D, R[4], NW, NH, "Redis", "세션 · cookie_t");
box(s2, D, R[5], NW, NH, "TimescaleDB", "quote_bars");

// column headers
const hdr = (x, t) => s2.addText(t, { x, y: 0.62, w: NW, h: 0.28, align: "center", fontSize: 10, bold: true, color: MUTED, fontFace: "Calibri" });
hdr(A, "외부 · 진입점"); hdr(B, "DMZ Edge"); hdr(Cc, "Internal 서비스"); hdr(D, "Broker · 저장소");

// --- C world band (bottom) ---
const bandY = 4.72, bandH = 1.02, bandX = 0.3, bandW = 9.45;
s2.addShape(pres.shapes.ROUNDED_RECTANGLE, { x: bandX, y: bandY, w: bandW, h: bandH, rectRadius: 0.06, fill: { color: "FBF2F7" }, line: { color: T.shm, width: 1.25 } });
s2.addText([
  { text: "C 세계 — broker·gRPC 미접촉", options: { bold: true, fontSize: 10, color: T.shm, breakLine: true } },
  { text: "같은 호스트 · 외부 의존 0 · 기존 C 코드가 mymq 없이 붙는 경로 (raw socket · HTTP · SHM)", options: { fontSize: 8.5, color: MUTED } },
], { x: bandX + 0.15, y: bandY + 0.14, w: 4.5, h: 0.72, valign: "middle", fontFace: "Calibri" });
// C nodes
box(s2, 5.15, 4.95, 2.15, 0.55, "cside 운영 svc", "wtgpush/query/price");
box(s2, 7.55, 4.95, 2.0, 0.55, "C algo (매매)", "same host");
// C connectors up
conn(s2, 6.2, 4.95, 6.15, rc(3), T.http);          // cside -> mci-push (HTTP)
conn(s2, 6.2, 4.95, cx(Cc, NW), rc(2) + 0.24, T.http); // cside -> mci-price (HTTP)
conn(s2, 8.5, 4.95, cx(Cc, NW) + 0.5, rc(2) + 0.24, T.shm, "dash"); // algo <-> price (SHM)

// --- legend panel (right) ---
const LX = 10.35, LY = 0.95, LW = 2.65;
s2.addShape(pres.shapes.ROUNDED_RECTANGLE, { x: LX, y: LY, w: LW, h: 4.79, rectRadius: 0.05, fill: { color: "F8FAFC" }, line: { color: NODEBORD, width: 1 } });
s2.addText("전송 기술 범례", { x: LX + 0.15, y: LY + 0.12, w: LW - 0.3, h: 0.3, fontSize: 11, bold: true, color: DARK, fontFace: "Calibri" });
const legend = [
  [T.udp, "UDP FIX 4.4", "시세 피드 수신"],
  [T.mymq, "MyMQ wire (TCP)", "서비스 ↔ broker"],
  [T.grpc, "gRPC (HTTP/2)", "Go ↔ Go 내부"],
  [T.http, "HTTP / REST", "edge·cside 경계"],
  [T.ws, "WebSocket", "고객 fan-out"],
  [T.fix, "FIX 4.4 (TCP)", "외부 카운터파티"],
  [T.pg, "PostgreSQL (pgx)", "봉 저장/조회"],
  [T.redis, "Redis (RESP)", "세션·cookie"],
  [T.shm, "SHM  (제안)", "동일 호스트 algo"],
];
let ly = LY + 0.52;
legend.forEach(([c, name, desc], i) => {
  s2.addShape(pres.shapes.LINE, { x: LX + 0.18, y: ly + 0.11, w: 0.42, h: 0, line: { color: c, width: 3, dashType: (name.indexOf("SHM") === 0 ? "dash" : "solid") } });
  s2.addText([
    { text: name + "  ", options: { bold: true, fontSize: 8.7, color: DARK } },
    { text: "· " + desc, options: { fontSize: 7.8, color: MUTED } },
  ], { x: LX + 0.68, y: ly - 0.06, w: LW - 0.8, h: 0.34, valign: "middle", fontFace: "Calibri", margin: 0 });
  ly += 0.455;
});

// =========================================================
// SLIDE 3 — Transport inventory table
// =========================================================
let s3 = pres.addSlide();
s3.background = { color: WHITE };
s3.addText("전송 기술 인벤토리 — 구간별 정확한 매핑",
  { x: 0.3, y: 0.22, w: 12.7, h: 0.5, fontSize: 20, bold: true, color: DARK, fontFace: "Cambria" });

const th = (t) => ({ text: t, options: { bold: true, color: WHITE, fill: { color: DARK }, fontSize: 11, align: "left", valign: "middle" } });
const chip = (c, t) => ({ text: t, options: { bold: true, color: WHITE, fill: { color: c }, align: "center", valign: "middle", fontSize: 9.5 } });
const td = (t, o) => ({ text: t, options: Object.assign({ fontSize: 9.5, color: DARK, valign: "middle", align: "left" }, o || {}) });

const rows = [
  [th("전송 기술"), th("구간 (누가 → 누가)"), th("성격 · 이유")],
  [chip(T.udp, "UDP FIX 4.4"), td("시장 피드 → quote-forwarder"), td("무연결 브로드캐스트 수신, 커널 drop 회피 위해 reader/worker 분리")],
  [chip(T.mymq, "MyMQ wire"), td("mci-api · price · push · admin · forwarder ↔ mymqd"), td("자체 바이너리(100B mqhdr+length-prefix), ckey 멀티플렉싱, FANOUT")],
  [chip(T.grpc, "gRPC / HTTP2"), td("edge-price→price, edge-push→push, price→chart, etcd client"), td("Go↔Go 내부 전용 — 스트리밍·deadline·LB·codegen 이득")],
  [chip(T.http, "HTTP / REST"), td("외부→edge-api→mci-api, edge-chart 프록시, cside C SDK, HTTP push"), td("TLS termination·역프록시. C 경계는 raw socket+HTTP(외부 의존 0)")],
  [chip(T.ws, "WebSocket"), td("외부 클라이언트 ↔ edge-price · edge-push · edge-chart"), td("고객 실시간 fan-out. 인스턴스당 cap 有 → 수평 확장")],
  [chip(T.fix, "FIX 4.4 / TCP"), td("외부 FIX 카운터파티 → mci-edge-fix → /v1/tx"), td("QuickFIX/Go 세션 종단, Logon 검증 후 alias 라우팅")],
  [chip(T.pg, "PostgreSQL"), td("mci-price(Archiver) INSERT · mci-chart(Repo) SELECT → TimescaleDB"), td("pgx/v5. 봉만 영속(raw tick 미저장), 압축·retention 정책")],
  [chip(T.redis, "Redis (RESP)"), td("mci-api · mci-push ↔ Redis"), td("세션 + cookie_t 공유(멀티 인스턴스), 재시작 복구")],
  [chip(T.shm, "SHM (제안)"), td("mci-price → 동일 호스트 C algo"), td("mds식 μs 지연 복원. seqlock + ts staleness 가드 필수")],
];
s3.addTable(rows, {
  x: 0.3, y: 0.9, w: 12.7,
  colW: [1.9, 5.0, 5.8],
  rowH: [0.34, 0.5, 0.62, 0.56, 0.62, 0.5, 0.5, 0.56, 0.44, 0.56],
  border: { pt: 0.5, color: "D9DEE5" }, fontFace: "Calibri",
  fill: { color: "FFFFFF" }, align: "left", valign: "middle", margin: 4,
});
s3.addText("raw tick은 저장하지 않고 conflation(심볼당 최신 1건)으로 전달 — 자세한 근거는 별도 논의 참조",
  { x: 0.3, y: 6.95, w: 12.7, h: 0.35, fontSize: 9, italic: true, color: MUTED, fontFace: "Calibri" });

// =========================================================
// SLIDE 4 — Principle
// =========================================================
let s4 = pres.addSlide();
s4.background = { color: DARK };
s4.addText("설계 원칙 — 경계마다 맞는 도구", { x: 0.6, y: 0.5, w: 12, h: 0.6, fontSize: 26, bold: true, color: WHITE, fontFace: "Cambria" });
s4.addText("gRPC를 전 구간에 강요하지 않는다. C가 만지는 경계엔 raw socket · HTTP · SHM만 노출한다.",
  { x: 0.6, y: 1.2, w: 12, h: 0.5, fontSize: 14, color: "CBD5E0", fontFace: "Calibri" });

const cards = [
  ["Go ↔ Go 내부", TEAL, "gRPC (HTTP/2)", "스트리밍·deadline·클라이언트 LB·codegen이 실제로 이득. mci-edge ↔ internal, price ↔ chart."],
  ["C ↔ 시스템 경계", T.http, "raw socket · HTTP", "cside SDK는 POSIX 소켓+외부 의존 0. protobuf·C++ 링크 강요 없음. broker(mymq) 의존 제거."],
  ["동일 호스트 algo", T.shm, "SHM (제안)", "네트워크·직렬화 우회 μs 지연. conflation은 공짜, 대신 ts 기반 staleness kill-switch 필수."],
  ["시세 대량 수신", T.udp, "UDP → broker", "피드는 UDP, 내부 배포는 MyMQ FANOUT broadcast. 고객단만 WebSocket fan-out."],
];
const cw = 5.95, ch = 2.0, gx = 0.6, gy = 0.35;
cards.forEach((c, i) => {
  const col = i % 2, row = Math.floor(i / 2);
  const x = 0.6 + col * (cw + gx), y = 2.05 + row * (ch + gy);
  s4.addShape(pres.shapes.ROUNDED_RECTANGLE, { x, y, w: cw, h: ch, rectRadius: 0.05, fill: { color: "26313F" }, line: { color: "3A4756", width: 1 }, shadow: shadow() });
  s4.addShape(pres.shapes.OVAL, { x: x + 0.28, y: y + 0.32, w: 0.22, h: 0.22, fill: { color: c[1] }, line: { type: "none" } });
  s4.addText(c[0], { x: x + 0.62, y: y + 0.22, w: cw - 0.9, h: 0.4, fontSize: 15, bold: true, color: WHITE, fontFace: "Calibri", valign: "middle" });
  s4.addText(c[2], { x: x + 0.3, y: y + 0.72, w: cw - 0.6, h: 0.4, fontSize: 13, bold: true, color: c[1], fontFace: "Calibri" });
  s4.addText(c[3], { x: x + 0.3, y: y + 1.12, w: cw - 0.6, h: 0.78, fontSize: 10.5, color: "C6CFDA", fontFace: "Calibri", valign: "top" });
});

pres.writeFile({ fileName: "wtg-ipc.pptx" }).then(() => console.log("done"));
