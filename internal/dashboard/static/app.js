// AETHERIS Control Plane — Canlı telemetri motoru
// Sıfır CDN, sıfır framework, tam offline, WebSocket + Canvas
(function () {
  "use strict";
  var bwHistory = Array(60).fill(0);
  var bwPeak = 1;
  var nodeCache = {};

  // --- Yardımcılar ---
  function $(id) { return document.getElementById(id); }
  function setText(id, v) { var el=$(id); if(el) el.textContent=v; }
  function fmtBytes(n) {
    n=Number(n)||0;
    if(n<1024) return n+"B";
    if(n<1048576) return (n/1024).toFixed(1)+"KB";
    if(n<1073741824) return (n/1048576).toFixed(1)+"MB";
    return (n/1073741824).toFixed(2)+"GB";
  }
  function fmtBps(n) { return fmtBytes(n)+"/s"; }
  function esc(s){return String(s||"").replace(/[&<>]/g,function(c){return{"&":"&amp;","<":"&lt;",">":"&gt;"}[c];}); }

  // --- WAN Hero ---
  var wanClasses = {
    direct:"direct", relayed:"relayed", off_grid:"isolated", unknown:"unknown"
  };
  var wanLabels = {
    direct:"● Direct WAN",
    relayed:"● Relayed via Peer",
    off_grid:"● Isolated Mesh Only",
    unknown:"● Unknown"
  };
  function renderWAN(status, exitPeer) {
    var badge=$("wan-badge"), pulse=$("wan-pulse");
    if(!badge) return;
    badge.className="wan-badge "+(wanClasses[status]||"unknown");
    var label=wanLabels[status]||"● Unknown";
    if(status==="relayed"&&exitPeer) label="● Relayed via "+exitPeer;
    setText("wan-text", label);
    var metaEl=$("wan-meta");
    if(metaEl) metaEl.textContent=exitPeer?"Exit peer: "+exitPeer:"";
  }

  // --- Taşıyıcı Boru Hattı ---
  var carrierMap = {
    "ethernet":"ethernet","wifi_wan":"wifi_wan",
    "wigig_60ghz_802_11ad":"wigig","fso_laser_optical":"fso",
    "wifi_halow_802_11ah":"halow","tvws_470_790mhz":"tvws",
    "lora_usb_serial":"lora","ble_mesh":"ble"
  };
  function renderCarrier(kind) {
    var items=document.querySelectorAll(".pipeline-item");
    items.forEach(function(item){
      var k=item.dataset.kind;
      var mapped=carrierMap[kind]||kind;
      item.className="pipeline-item "+(k===mapped?"active":"standby");
      var st=item.querySelector(".pi-status");
      if(st){
        if(k===mapped){st.className="pi-status active";st.textContent="AKTİF";}
        else{st.className="pi-status stub";st.textContent="—";}
      }
    });
    // Carrier pill topbar
    var labels={"ethernet":"Ethernet GbE","wifi_wan":"Wi-Fi WAN",
      "wigig_60ghz_802_11ad":"WiGig 60GHz","fso_laser_optical":"FSO Lazer",
      "wifi_halow_802_11ah":"HaLow 802.11ah","tvws_470_790mhz":"TVWS 470MHz",
      "lora_usb_serial":"LoRa 868MHz","ble_mesh":"BLE Mesh","ip":"IP"};
    setText("carrier-label", labels[kind]||kind.toUpperCase());
  }

  // --- Mesh Topoloji Canvas ---
  function drawTopology(nodes) {
    var cv=$("topo-canvas"); if(!cv) return;
    var ctx=cv.getContext("2d"), W=cv.width, H=cv.height;
    ctx.clearRect(0,0,W,H);
    if(!nodes.length){
      ctx.fillStyle="rgba(71,85,105,.4)";ctx.font="12px system-ui";
      ctx.textAlign="center";ctx.fillText("mesh bekleniyor…",W/2,H/2);return;
    }
    var cx=W/2, cy=H/2, R=Math.min(W,H)/2-50;
    // Merkez düğüm (self)
    drawNode(ctx,cx,cy,nodes[0],"#06b6d4",true);
    // Kenar + dış düğümler
    for(var i=1;i<nodes.length;i++){
      var ang=(i-1)/(Math.max(1,nodes.length-1))*Math.PI*2 - Math.PI/2;
      var x=cx+Math.cos(ang)*R, y=cy+Math.sin(ang)*R;
      // Bağlantı çizgisi
      ctx.beginPath();
      var grad=ctx.createLinearGradient(cx,cy,x,y);
      grad.addColorStop(0,"rgba(59,130,246,.5)");
      grad.addColorStop(1,"rgba(6,182,212,.2)");
      ctx.strokeStyle=grad;ctx.lineWidth=1.5;
      ctx.setLineDash([4,4]);ctx.stroke();ctx.setLineDash([]);
      ctx.beginPath();ctx.moveTo(cx,cy);ctx.lineTo(x,y);ctx.stroke();
      drawNode(ctx,x,y,nodes[i],nodes[i].alive===false?"#ef4444":"#3b82f6",false);
    }
  }
  function drawNode(ctx,x,y,n,color,isCenter){
    var r=isCenter?10:7;
    // Glow
    ctx.beginPath();ctx.arc(x,y,r+6,0,Math.PI*2);
    ctx.fillStyle="rgba("+hexToRgb(color)+",.1)";ctx.fill();
    // Çember
    ctx.beginPath();ctx.arc(x,y,r,0,Math.PI*2);
    ctx.fillStyle=color;ctx.fill();
    ctx.strokeStyle="rgba(255,255,255,.15)";ctx.lineWidth=1.5;ctx.stroke();
    // Etiket
    ctx.fillStyle="#e2e8f0";ctx.font=(isCenter?"bold ":"")+"11px 'JetBrains Mono',monospace";
    ctx.textAlign="center";ctx.fillText(String(n.id||"").slice(0,12),x,y-r-6);
  }
  function hexToRgb(h){
    var r={"#06b6d4":"6,182,212","#3b82f6":"59,130,246","#ef4444":"239,68,68"};
    return r[h]||"255,255,255";
  }

  // --- Bant Genişliği Sparkline ---
  function drawSparkline(bps) {
    bwHistory.push(bps);bwHistory.shift();
    bwPeak=Math.max(1,...bwHistory);
    var cv=$("bw-canvas"); if(!cv) return;
    var ctx=cv.getContext("2d"), W=cv.width, H=cv.height;
    ctx.clearRect(0,0,W,H);
    var step=W/bwHistory.length;
    // Gradient fill
    var grad=ctx.createLinearGradient(0,0,0,H);
    grad.addColorStop(0,"rgba(59,130,246,.3)");
    grad.addColorStop(1,"rgba(59,130,246,.02)");
    ctx.beginPath();ctx.moveTo(0,H);
    bwHistory.forEach(function(v,i){
      var px=i*step, py=H-(v/bwPeak)*H*.85;
      i===0?ctx.lineTo(px,py):ctx.lineTo(px,py);
    });
    ctx.lineTo(W,H);ctx.closePath();ctx.fillStyle=grad;ctx.fill();
    // Line
    ctx.beginPath();ctx.strokeStyle="#3b82f6";ctx.lineWidth=2;
    bwHistory.forEach(function(v,i){
      var px=i*step, py=H-(v/bwPeak)*H*.85;
      i===0?ctx.moveTo(px,py):ctx.lineTo(px,py);
    });
    ctx.stroke();
    // Peak label
    if(bwPeak>1){
      ctx.fillStyle="rgba(148,163,184,.6)";ctx.font="9px system-ui";
      ctx.textAlign="right";ctx.fillText(fmtBps(bwPeak),W-4,12);
    }
  }

  // --- Node tablosu ---
  function renderNodes(nodes){
    var tb=$("node-rows");if(!tb)return;
    tb.innerHTML="";
    nodes.forEach(function(n){
      var tr=document.createElement("tr");
      var rtt=n.rtt_ms?n.rtt_ms.toFixed(1)+"ms":"—";
      var pill=n.alive===false
        ?"<span class='status-pill down'>inaktif</span>"
        :"<span class='status-pill up'>aktif</span>";
      tr.innerHTML="<td>"+esc(n.id)+"</td><td>"+esc(n.carrier||"ip")+"</td><td>"+rtt+"</td><td>"+pill+"</td>";
      tb.appendChild(tr);
    });
    if(!nodes.length){
      tb.innerHTML="<tr><td colspan='4' style='color:var(--faint);text-align:center;padding:16px'>mesh düğümü bekleniyor…</td></tr>";
    }
  }

  // --- Kredi tablosu ---
  function renderCredits(rows){
    var tb=$("credit-rows");if(!tb)return;
    tb.innerHTML="";
    (rows||[]).forEach(function(r){
      var tr=document.createElement("tr");
      tr.innerHTML="<td>"+esc(r.client_id)+"</td><td>"+fmtBytes(r.bytes||0)+"</td>";
      tb.appendChild(tr);
    });
    if(!(rows&&rows.length)){
      tb.innerHTML="<tr><td colspan='2' style='color:var(--faint);padding:10px'>kayıt yok</td></tr>";
    }
  }

  // --- Ana render ---
  function render(d) {
    renderWAN(d.wan_status, d.exit_peer);
    renderCarrier(d.active_carrier||"ip");
    setText("hero-tunnels", d.active_tunnels||0);
    setText("hero-carrier", (d.active_carrier||"ip").toUpperCase());
    var nodes=d.nodes||[];
    var rtts=nodes.filter(function(n){return n.rtt_ms>0;}).map(function(n){return n.rtt_ms;});
    setText("hero-rtt", rtts.length?(rtts.reduce(function(a,b){return a+b;},0)/rtts.length).toFixed(1)+"ms":"—");
    var bps=d.throughput_bps||0;
    setText("m-bw", fmtBps(bps));
    var fill=$("bw-fill");if(fill)fill.style.width=Math.min(100,(bps/1000000)*100)+"%";
    setText("m-wal", d.wal_depth||0);
    setText("m-disk", fmtBytes(d.disk_bytes));
    setText("m-nodes", nodes.length);
    if(d.socks5) setText("m-socks5", d.socks5.active);
    if(d.dtn)    setText("m-dtn",    d.dtn.pending);
    setText("ts", d.ts?new Date(d.ts*1000).toLocaleTimeString():"—");
    renderNodes(nodes);
    renderCredits(d.credits);
    drawTopology(nodes);
    drawSparkline(bps);
  }

  // --- WebSocket bağlantısı ---
  function connect(){
    var proto=location.protocol==="https:"?"wss:":"ws:";
    var token=new URLSearchParams(location.search).get("token")||"";
    var ws=new WebSocket(proto+"//"+location.host+"/api/v1/ws/telemetry?token="+encodeURIComponent(token));
    ws.onopen=function(){
      var d=$("conn-dot"); if(d) d.className="dot on";
      setText("conn-text","canlı — telemetri akıyor");
    };
    ws.onclose=function(){
      var d=$("conn-dot"); if(d) d.className="dot off";
      setText("conn-text","yeniden bağlanıyor…");
      setTimeout(connect,2500);
    };
    ws.onmessage=function(ev){try{render(JSON.parse(ev.data));}catch(e){}};
  }
  connect();
})();
