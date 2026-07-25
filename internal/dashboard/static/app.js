// AETHERIS yönetim paneli istemcisi — saf vanilla JS, dış kütüphane yok.
// WebSocket üzerinden canlı telemetri alır ve DOM/canvas'a çizer.
(function () {
  "use strict";

  var dot = document.getElementById("dot");
  var connText = document.getElementById("conn-text");
  var canvas = document.getElementById("topo");
  var ctx = canvas.getContext("2d");

  // Admin oturum token'ı URL'den (?token=...) alınır; WebSocket'e aktarılır.
  function token() {
    var m = /[?&]token=([^&]+)/.exec(window.location.search);
    return m ? decodeURIComponent(m[1]) : "";
  }

  function wsURL() {
    var proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    var t = token();
    var q = t ? "?token=" + encodeURIComponent(t) : "";
    return proto + "//" + window.location.host + "/api/v1/ws/telemetry" + q;
  }

  function fmtBytes(n) {
    if (n === undefined || n === null) return "—";
    var u = ["B", "KB", "MB", "GB", "TB"], i = 0, v = Number(n);
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(v < 10 && i > 0 ? 1 : 0) + " " + u[i];
  }

  function setConn(on) {
    dot.className = "dot " + (on ? "on" : "off");
    connText.textContent = on ? "canlı — telemetri akıyor" : "bağlantı koptu, yeniden deneniyor…";
  }

  function render(d) {
    setText("wal-depth", d.wal_depth);
    setText("tunnels", d.active_tunnels);
    setText("disk", fmtBytes(d.disk_bytes));
    setText("throughput", fmtBytes(d.throughput_bps) + "/s");
    setText("ts", d.ts ? new Date(d.ts * 1000).toLocaleTimeString() : "—");

    renderWAN(d.wan_status, d.wan_label, d.exit_peer);
    renderNodes(d.nodes || []);
    renderCredits(d.credits || []);
    drawTopology(d.nodes || []);
  }

  // WAN durumu göstergesi: Direct / Relayed / Off-Grid.
  function renderWAN(status, label, exitPeer) {
    var el = document.getElementById("wan-badge");
    if (!el) return;
    var cls = "wan-unknown", text = "WAN: " + (label || "—");
    if (status === "direct") cls = "wan-direct";
    else if (status === "relayed") {
      cls = "wan-relayed";
      if (exitPeer) text += " (" + exitPeer + ")";
    } else if (status === "off_grid") cls = "wan-offgrid";
    el.className = "wan " + cls;
    el.textContent = text;
  }

  function setText(id, v) {
    var el = document.getElementById(id);
    if (el) el.textContent = (v === undefined || v === null) ? "—" : v;
  }

  function renderNodes(nodes) {
    var tb = document.getElementById("node-rows");
    tb.innerHTML = "";
    nodes.forEach(function (n) {
      var tr = document.createElement("tr");
      tr.appendChild(td(n.id));
      tr.appendChild(td(n.carrier || "—"));
      tr.appendChild(td(n.rtt_ms != null ? n.rtt_ms.toFixed(1) + " ms" : "—"));
      var s = document.createElement("td");
      var b = document.createElement("span");
      b.className = "badge " + (n.alive ? "up" : "down");
      b.textContent = n.alive ? "aktif" : "kayıp";
      s.appendChild(b);
      tr.appendChild(s);
      tb.appendChild(tr);
    });
  }

  function renderCredits(rows) {
    var tb = document.getElementById("credit-rows");
    tb.innerHTML = "";
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      tr.appendChild(td(r.client_id));
      tr.appendChild(td(String(r.units)));
      tr.appendChild(td(fmtBytes(r.bytes)));
      tb.appendChild(tr);
    });
  }

  function td(text) {
    var c = document.createElement("td");
    c.textContent = text;
    return c;
  }

  // Basit dairesel topoloji çizimi: düğümler çember üzerinde, merkeze bağlı.
  function drawTopology(nodes) {
    var W = canvas.width, H = canvas.height;
    ctx.clearRect(0, 0, W, H);
    var cx = W / 2, cy = H / 2, R = Math.min(W, H) / 2 - 50;

    // merkez
    ctx.fillStyle = "#4aa3ff";
    ctx.beginPath(); ctx.arc(cx, cy, 8, 0, Math.PI * 2); ctx.fill();

    var n = nodes.length;
    if (n === 0) return;
    for (var i = 0; i < n; i++) {
      var a = (i / n) * Math.PI * 2 - Math.PI / 2;
      var x = cx + Math.cos(a) * R, y = cy + Math.sin(a) * R;
      // bağlantı çizgisi (RTT'ye göre kalınlık)
      ctx.strokeStyle = nodes[i].alive ? "rgba(53,208,165,.5)" : "rgba(229,72,77,.4)";
      ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(cx, cy); ctx.lineTo(x, y); ctx.stroke();
      // düğüm
      ctx.fillStyle = nodes[i].alive ? "#35d0a5" : "#e5484d";
      ctx.beginPath(); ctx.arc(x, y, 7, 0, Math.PI * 2); ctx.fill();
      // etiket
      ctx.fillStyle = "#d7e0ea";
      ctx.font = "11px system-ui, sans-serif";
      ctx.textAlign = "center";
      ctx.fillText(nodes[i].id, x, y - 12);
    }
  }

  var ws = null;
  var retry = null;

  function connect() {
    try {
      ws = new WebSocket(wsURL());
    } catch (e) {
      scheduleRetry();
      return;
    }
    ws.onopen = function () { setConn(true); };
    ws.onmessage = function (ev) {
      try { render(JSON.parse(ev.data)); } catch (e) { /* yut */ }
    };
    ws.onclose = function () { setConn(false); scheduleRetry(); };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
  }

  function scheduleRetry() {
    if (retry) return;
    retry = setTimeout(function () { retry = null; connect(); }, 2000);
  }

  connect();
})();
