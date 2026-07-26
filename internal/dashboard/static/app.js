// AETHERIS Control Plane — canli telemetri (stdlib WebSocket, sifir bagimlilik).
(function () {
  "use strict";

  function setText(id, v) { var el = document.getElementById(id); if (el) el.textContent = v; }

  function fmtBytes(n) {
    n = Number(n) || 0;
    var u = ["B", "KB", "MB", "GB", "TB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
  }

  // WAN durumu: Direct WAN / Relayed via [Peer-ID] / Isolated Mesh Only.
  function renderWAN(status, label, exitPeer) {
    var badge = document.getElementById("wan-badge");
    var text = document.getElementById("wan-text");
    if (!badge || !text) return;
    var cls = "wan-unknown", msg = label || "—";
    if (status === "direct") { cls = "wan-direct"; msg = "Direct WAN"; }
    else if (status === "relayed") {
      cls = "wan-relayed";
      msg = exitPeer ? ("Relayed via " + exitPeer) : "Relayed via Peer";
    } else if (status === "off_grid") { cls = "wan-offgrid"; msg = "Isolated Mesh Only"; }
    badge.className = "wan-badge " + cls;
    text.textContent = msg;
  }

  function render(d) {
    renderWAN(d.wan_status, d.wan_label, d.exit_peer);
    setText("carrier", (d.active_carrier || "ip").toUpperCase());
    setText("tunnels", d.active_tunnels || 0);
    setText("throughput", fmtBytes(d.throughput_bps) + "/s");
    setText("wal-depth", d.wal_depth || 0);
    setText("disk", fmtBytes(d.disk_bytes));

    var nodes = d.nodes || [];
    setText("nodecount", nodes.length);
    // Ortalama RTT (canli dugumler).
    var rtts = nodes.filter(function (n) { return n.rtt_ms > 0; }).map(function (n) { return n.rtt_ms; });
    setText("rtt", rtts.length ? (rtts.reduce(function (a, b) { return a + b; }, 0) / rtts.length).toFixed(1) + " ms" : "—");

    setText("ts", d.ts ? new Date(d.ts * 1000).toLocaleTimeString() : "—");
    renderNodes(nodes);
    renderCredits(d.credits || []);
    drawTopology(nodes);
  }

  function renderNodes(nodes) {
    var tb = document.getElementById("node-rows");
    if (!tb) return;
    tb.innerHTML = "";
    nodes.forEach(function (n) {
      var tr = document.createElement("tr");
      var rtt = n.rtt_ms ? n.rtt_ms.toFixed(1) + " ms" : "—";
      var pill = n.alive ? '<span class="pill pill-up">aktif</span>' : '<span class="pill pill-down">yok</span>';
      tr.innerHTML = "<td>" + esc(n.id) + "</td><td>" + esc(n.carrier || "-") + "</td><td>" + rtt + "</td><td>" + pill + "</td>";
      tb.appendChild(tr);
    });
    if (!nodes.length) tb.innerHTML = '<tr><td colspan="4" style="color:var(--faint)">düğüm bekleniyor…</td></tr>';
  }

  function renderCredits(rows) {
    var tb = document.getElementById("credit-rows");
    if (!tb) return;
    tb.innerHTML = "";
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      tr.innerHTML = "<td>" + esc(r.client_id) + "</td><td>" + fmtBytes(r.bytes) + "</td>";
      tb.appendChild(tr);
    });
    if (!rows.length) tb.innerHTML = '<tr><td colspan="2" style="color:var(--faint)">kayıt yok</td></tr>';
  }

  function esc(s) { return String(s == null ? "" : s).replace(/[&<>]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]; }); }

  function drawTopology(nodes) {
    var cv = document.getElementById("topo");
    if (!cv || !cv.getContext) return;
    var ctx = cv.getContext("2d");
    var W = cv.width, H = cv.height;
    ctx.clearRect(0, 0, W, H);
    if (!nodes.length) return;
    var cx = W / 2, cy = H / 2, R = Math.min(W, H) / 2 - 46;
    var self = nodes[0];
    // kenarlar
    ctx.strokeStyle = "rgba(59,130,246,.35)"; ctx.lineWidth = 1.5;
    for (var i = 1; i < nodes.length; i++) {
      var a = (i - 1) / Math.max(1, nodes.length - 1) * Math.PI * 2;
      var x = cx + Math.cos(a) * R, y = cy + Math.sin(a) * R;
      ctx.beginPath(); ctx.moveTo(cx, cy); ctx.lineTo(x, y); ctx.stroke();
    }
    // dis dugumler
    for (var j = 1; j < nodes.length; j++) {
      var ang = (j - 1) / Math.max(1, nodes.length - 1) * Math.PI * 2;
      var nx = cx + Math.cos(ang) * R, ny = cy + Math.sin(ang) * R;
      dot(ctx, nx, ny, nodes[j], "#3b82f6");
    }
    // merkez (self)
    dot(ctx, cx, cy, self, "#10b981");
  }
  function dot(ctx, x, y, n, color) {
    ctx.beginPath(); ctx.arc(x, y, 7, 0, Math.PI * 2);
    ctx.fillStyle = n.alive === false ? "#ef4444" : color; ctx.fill();
    ctx.fillStyle = "#e2e8f0"; ctx.font = "11px system-ui"; ctx.textAlign = "center";
    ctx.fillText(String(n.id || ""), x, y - 12);
  }

  function connect() {
    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var token = new URLSearchParams(location.search).get("token") || "";
    var ws = new WebSocket(proto + "//" + location.host + "/api/v1/ws/telemetry?token=" + encodeURIComponent(token));
    ws.onopen = function () {
      document.getElementById("conn-dot").className = "dot on";
      setText("conn-text", "canlı — telemetri akıyor");
    };
    ws.onclose = function () {
      document.getElementById("conn-dot").className = "dot off";
      setText("conn-text", "bağlantı koptu — yeniden deneniyor");
      setTimeout(connect, 2000);
    };
    ws.onmessage = function (ev) {
      try { render(JSON.parse(ev.data)); } catch (e) {}
    };
  }
  connect();
})();
