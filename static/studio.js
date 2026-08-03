const $ = (id) => document.getElementById(id);
const signedIn = $("studio").dataset.signedIn === "true";
let projectID = "", files = {}, activePath = "index.html", agentText = "", previewTimer;
let cloudRevision = "", mainlineRevision = "", viewingVersion = "", shareToken = "", readOnlyShare = false;
let lastSavedSnapshot = "", lastPublishedSnapshot = "", saveTimer = 0, draftTimer = 0, savePromise = null, changeSerial = 0;
let agentRunID = "", agentSession = "";
let editor, editorUpdating = false;
const DRAFT_PREFIX = "mojojojo.studio.draft.v1:";
const SAVE_DEBOUNCE_MS = 8000, DRAFT_DEBOUNCE_MS = 600;

const commonCSS = `:root{font-family:Inter,system-ui,sans-serif;color:#211b2b;background:#faf8ff}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;background:radial-gradient(circle at 70% 0,#e8f8eb,transparent 45%),#faf8ff}.card{width:min(680px,100%);background:#fff;border:1px solid #ddd5e8;border-radius:20px;padding:28px;box-shadow:0 20px 60px #392b5120}h1{margin:0 0 8px;font-size:30px}p{color:#675d72}button,.drop{border:1px solid #cfc3dc;border-radius:10px;padding:11px 15px;background:#f7f3fb;font:inherit}button{cursor:pointer;background:#211b2b;color:#fff}.drop{display:block;margin:18px 0;border-style:dashed;text-align:center}textarea{width:100%;min-height:170px;padding:12px;border:1px solid #cfc3dc;border-radius:10px;font:13px/1.5 ui-monospace,monospace}.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.result{margin-top:16px;padding:12px;border-radius:10px;background:#f2f8f3;white-space:pre-wrap;overflow:auto}a{color:#584170}`;

const templates = {
  "snake": {
    "index.html": `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Streaming Snake</title><link rel="stylesheet" href="app.css"></head><body><main class="game-shell"><header><div><p class="eyebrow">PEER-TO-PEER ARCADE</p><h1>Streaming Snake</h1></div><div id="network" class="network">Offline</div></header><section id="lobby" class="lobby"><label>Player name<input id="player-name" maxlength="24" value="Player"></label><label>Room code<input id="room-code" maxlength="12" placeholder="SNAKE7"></label><div class="actions"><button id="create-room">Create room</button><button id="join-room" class="secondary">Join room</button></div><p id="lobby-note">Create a room, copy the invite, and play while the Python broker connects your browsers.</p></section><section id="game" class="game" hidden><div class="game-top"><div><span class="label">ROOM</span><strong id="room-label"></strong></div><button id="copy-invite" class="secondary">Copy invite</button><button id="refresh-lobby" class="quiet">Find players</button></div><canvas id="board" width="960" height="660" aria-label="Multiplayer snake board"></canvas><div class="under-board"><div id="players" class="players"></div><p id="status">Waiting for another snake…</p></div><div class="dpad" aria-label="Touch controls"><button data-direction="up">↑</button><button data-direction="left">←</button><button data-direction="down">↓</button><button data-direction="right">→</button></div></section><footer>The serverless function only brokers the handshake. Snake positions stream directly between players over encrypted WebRTC data channels.</footer></main><script src="app.js"></script></body></html>`,
    "app.css": `:root{font-family:Inter,ui-sans-serif,system-ui,sans-serif;color:#d9dee7;background:#282c34}*{box-sizing:border-box}[hidden]{display:none!important}body{margin:0;min-height:100vh;padding:24px;background:radial-gradient(circle at 20% 0,#34483b 0,transparent 38%),radial-gradient(circle at 100% 70%,#493654 0,transparent 42%),#282c34}.game-shell{width:min(980px,100%);margin:auto}header,.game-top,.under-board{display:flex;align-items:center;justify-content:space-between;gap:14px}h1{font-size:clamp(32px,7vw,64px);line-height:.95;margin:4px 0 22px;letter-spacing:-.05em}.eyebrow,.label{font:11px ui-monospace,monospace;letter-spacing:.14em;color:#98c379}.network{padding:7px 11px;border:1px solid #4b5361;border-radius:999px;color:#abb2bf;font-size:12px}.network.live{color:#98c379;border-color:#6f995d}.lobby{display:grid;grid-template-columns:1fr 1fr auto;gap:14px;padding:20px;border:1px solid #4b5361;border-radius:18px;background:#323842}.lobby label{display:grid;gap:6px;color:#abb2bf;font-size:12px}.lobby input{width:100%;padding:11px 12px;border:1px solid #626c7b;border-radius:9px;background:#22262e;color:#d9dee7;font:15px ui-monospace,monospace;text-transform:none}.actions{display:flex;align-items:end;gap:8px}.lobby p{grid-column:1/-1;margin:0;color:#9aa3b2;font-size:13px}button{border:0;border-radius:9px;padding:11px 14px;background:#98c379;color:#20252c;font-weight:750;cursor:pointer}.secondary,.quiet{background:#39414d;color:#c5ccd7;border:1px solid #626c7b}.quiet{padding:8px 10px}.game{display:grid;gap:12px}.game-top{justify-content:flex-start}.game-top div{margin-right:auto;display:grid}.game-top strong{font:24px ui-monospace,monospace;letter-spacing:.12em}canvas{display:block;width:100%;aspect-ratio:32/22;border:1px solid #4b5361;border-radius:18px;background:#22262e;box-shadow:0 24px 70px rgba(18,21,26,.48)}.under-board{align-items:flex-start}.players{display:flex;gap:8px;flex-wrap:wrap}.player{padding:5px 9px;border:1px solid #4b5361;border-radius:999px;font-size:11px}.player::before{content:'';display:inline-block;width:7px;height:7px;margin-right:6px;border-radius:50%;background:var(--player)}#status{margin:5px 0;color:#abb2bf;font-size:12px}.dpad{display:none;grid-template-columns:repeat(3,46px);justify-content:center;gap:6px}.dpad button{padding:10px}.dpad button:nth-child(1){grid-column:2}.dpad button:nth-child(2){grid-column:1}.dpad button:nth-child(3){grid-column:2}.dpad button:nth-child(4){grid-column:3}footer{margin:20px 0;color:#9aa3b2;font-size:12px;max-width:75ch}@media(max-width:720px){body{padding:14px}.lobby{grid-template-columns:1fr}.lobby p{grid-column:auto}.actions{align-items:center}.game-top{flex-wrap:wrap}.dpad{display:grid}.under-board{display:grid}.network{align-self:flex-start}h1{margin-bottom:14px}}`,
    "app.js": `const $=selector=>document.querySelector(selector);
const COLORS=['#98e3ad','#e7a1b5','#8fcbd3','#e3b790','#bca9dc','#d6dba0','#ff8f70','#78d6c6'];
const peerID='peer_'+[...crypto.getRandomValues(new Uint32Array(3))].map(value=>value.toString(36)).join('');
const peers=new Map(),remoteSnakes=new Map();
let room='',cursor=0,pollTimer=0,pollUntil=0,direction={x:1,y:0},nextDirection=direction,score=0;
let snake=[{x:10,y:11},{x:9,y:11},{x:8,y:11}],food={x:24,y:11};
const canvas=$('#board'),context=canvas.getContext('2d');

function randomRoom(){const alphabet='ABCDEFGHJKLMNPQRSTUVWXYZ23456789',bytes=crypto.getRandomValues(new Uint8Array(6));return [...bytes].map(value=>alphabet[value%alphabet.length]).join('')}
function playerName(){return $('#player-name').value.trim().slice(0,24)||'Player'}
async function broker(payload){
  if(window.mojojojo)return window.mojojojo.invoke('broker',payload);
  const response=await fetch('./api/broker',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
  const result=await response.json();if(!response.ok)throw new Error(result.error||'Broker unavailable');return result;
}
async function enterRoom(code){
  room=code.toUpperCase().replace(/[^A-Z0-9]/g,'').slice(0,12);if(room.length<4)throw new Error('Enter at least four letters or digits');
  $('#room-code').value=room;$('#room-label').textContent=room;$('#lobby-note').textContent='Connecting…';
  const snapshot=await broker({action:'join',room,peer:peerID,name:playerName(),since:cursor});
  $('#lobby').hidden=true;$('#game').hidden=false;$('#network').textContent='Broker online';$('#network').classList.add('live');
  const url=new URL(location.href);url.searchParams.set('room',room);history.replaceState({},'',url);
  pollUntil=Date.now()+120000;await consumeSnapshot(snapshot);schedulePoll(300);
}
function schedulePoll(delay=1400){clearTimeout(pollTimer);if(Date.now()>pollUntil)return;pollTimer=setTimeout(pollBroker,delay)}
async function pollBroker(){
  try{const snapshot=await broker({action:'poll',room,peer:peerID,since:cursor});await consumeSnapshot(snapshot);schedulePoll()}
  catch(error){$('#status').textContent=error.message;schedulePoll(3000)}
}
async function consumeSnapshot(snapshot){
  cursor=Math.max(cursor,Number(snapshot.next||0));renderRoster(snapshot.peers||[]);
  for(const message of snapshot.signals||[])await receiveSignal(message.from,message.signal);
  for(const peer of snapshot.peers||[])if(peer.id!==peerID&&!peers.has(peer.id)&&peerID<peer.id)await createConnection(peer.id,true);
}
async function sendSignal(to,signal){await broker({action:'signal',room,peer:peerID,to,signal})}
async function createConnection(remoteID,initiator=false){
  if(peers.has(remoteID))return peers.get(remoteID);
  const pc=new RTCPeerConnection({iceServers:[{urls:'stun:stun.cloudflare.com:3478'},{urls:'stun:stun.l.google.com:19302'}]});
  const state={pc,channel:null,pending:[]};peers.set(remoteID,state);
  pc.onicecandidate=event=>{if(event.candidate)sendSignal(remoteID,{candidate:event.candidate}).catch(()=>{})};
  pc.ondatachannel=event=>setupChannel(remoteID,state,event.channel);
  pc.onconnectionstatechange=()=>{if(['failed','closed','disconnected'].includes(pc.connectionState)){peers.delete(remoteID);remoteSnakes.delete(remoteID);renderPlayers()}};
  if(initiator){setupChannel(remoteID,state,pc.createDataChannel('snake',{ordered:false,maxRetransmits:0}));const offer=await pc.createOffer();await pc.setLocalDescription(offer);await sendSignal(remoteID,{description:pc.localDescription})}
  return state;
}
function setupChannel(remoteID,state,channel){
  state.channel=channel;channel.onopen=()=>{$('#status').textContent='Peer-to-peer stream live';$('#network').textContent='P2P live';broadcastState();renderPlayers()};
  channel.onmessage=event=>{try{const message=JSON.parse(event.data);if(message.type==='state'){remoteSnakes.set(remoteID,message);renderPlayers()}}catch{}};
  channel.onclose=()=>{remoteSnakes.delete(remoteID);renderPlayers()};
}
async function receiveSignal(from,signal){
  const state=await createConnection(from,false),pc=state.pc;
  if(signal.description){await pc.setRemoteDescription(signal.description);for(const candidate of state.pending.splice(0))await pc.addIceCandidate(candidate);if(signal.description.type==='offer'){const answer=await pc.createAnswer();await pc.setLocalDescription(answer);await sendSignal(from,{description:pc.localDescription})}}
  if(signal.candidate){if(pc.remoteDescription)await pc.addIceCandidate(signal.candidate);else state.pending.push(signal.candidate)}
}
function renderRoster(roomPeers){$('#players').dataset.roomPeers=JSON.stringify(roomPeers);renderPlayers()}
function renderPlayers(){
  let roomPeers=[];try{roomPeers=JSON.parse($('#players').dataset.roomPeers||'[]')}catch{}
  $('#players').replaceChildren(...roomPeers.map((peer,index)=>{const item=document.createElement('span');item.className='player';item.style.setProperty('--player',COLORS[index%COLORS.length]);item.textContent=peer.name+(peer.id===peerID?' (you)':peers.get(peer.id)?.channel?.readyState==='open'?' · live':' · joining');return item}));
}
function broadcastState(){const message=JSON.stringify({type:'state',snake,food,score,name:playerName(),color:COLORS[hash(peerID)%COLORS.length]});for(const state of peers.values())if(state.channel?.readyState==='open')state.channel.send(message)}
function hash(value){let result=0;for(const char of value)result=(result*31+char.charCodeAt(0))>>>0;return result}
function gameTick(){
  direction=nextDirection;const head={x:(snake[0].x+direction.x+32)%32,y:(snake[0].y+direction.y+22)%22};
  if(snake.some(part=>part.x===head.x&&part.y===head.y)){snake=[{x:10,y:11},{x:9,y:11},{x:8,y:11}];score=0}else{snake.unshift(head);if(head.x===food.x&&head.y===food.y){score++;food={x:Math.floor(Math.random()*32),y:Math.floor(Math.random()*22)}}else snake.pop()}
  broadcastState();draw();
}
function drawSnake(parts,color){context.fillStyle=color;for(const part of parts){context.beginPath();context.roundRect(part.x*30+3,part.y*30+3,24,24,7);context.fill()}}
function draw(){
  context.clearRect(0,0,canvas.width,canvas.height);context.strokeStyle='#1c1725';context.lineWidth=1;
  for(let x=0;x<=32;x++){context.beginPath();context.moveTo(x*30,0);context.lineTo(x*30,660);context.stroke()}
  for(let y=0;y<=22;y++){context.beginPath();context.moveTo(0,y*30);context.lineTo(960,y*30);context.stroke()}
  context.fillStyle='#f0d77e';context.beginPath();context.arc(food.x*30+15,food.y*30+15,7,0,Math.PI*2);context.fill();drawSnake(snake,COLORS[hash(peerID)%COLORS.length]);
  for(const [id,state] of remoteSnakes)drawSnake(state.snake||[],state.color||COLORS[hash(id)%COLORS.length]);
}
function steer(x,y){if(direction.x+x===0&&direction.y+y===0)return;nextDirection={x,y}}
addEventListener('keydown',event=>{const moves={ArrowUp:[0,-1],ArrowDown:[0,1],ArrowLeft:[-1,0],ArrowRight:[1,0]};if(moves[event.key]){event.preventDefault();steer(...moves[event.key])}});
document.querySelectorAll('[data-direction]').forEach(button=>button.onclick=()=>{const moves={up:[0,-1],down:[0,1],left:[-1,0],right:[1,0]};steer(...moves[button.dataset.direction])});
$('#create-room').onclick=()=>enterRoom(randomRoom()).catch(error=>$('#lobby-note').textContent=error.message);
$('#join-room').onclick=()=>enterRoom($('#room-code').value).catch(error=>$('#lobby-note').textContent=error.message);
function inviteURL(){
  try{const url=new URL(window.mojojojo?.hostURL||location.href);if(!/^https?:$/.test(url.protocol))return room;url.searchParams.set('room',room);return url.href}catch{return room}
}
async function copyInvite(){
  const button=$('#copy-invite');button.textContent='Preparing invite…';let value;
  try{value=window.mojojojo?.canPublish?await window.mojojojo.invite(room):inviteURL()}catch(error){button.textContent='Invite failed';setTimeout(()=>button.textContent='Copy invite',1800);return}
  let copied=false;
  try{if(navigator.clipboard?.writeText){await navigator.clipboard.writeText(value);copied=true}}catch{}
  if(!copied){const field=document.createElement('textarea');field.value=value;field.readOnly=true;field.style.cssText='position:fixed;opacity:0;pointer-events:none';document.body.append(field);field.select();field.setSelectionRange(0,field.value.length);try{copied=document.execCommand('copy')}catch{}field.remove()}
  button.textContent=copied?'Invite copied':'Copy blocked';if(!copied)prompt('Copy this invite',value);setTimeout(()=>button.textContent='Copy invite',1800);
}
$('#copy-invite').onclick=copyInvite;
$('#refresh-lobby').onclick=()=>{pollUntil=Date.now()+120000;schedulePoll(0);$('#status').textContent='Looking for players…'};
let invited='';try{invited=new URL(window.mojojojo?.hostURL||location.href).searchParams.get('room')||''}catch{}if(invited)$('#room-code').value=invited.toUpperCase();
setInterval(gameTick,120);draw();`,
    "functions/broker.py": `def handler(request):\n    body = request.get("body") or {}\n    action = str(body.get("action", "")).lower()\n    if action not in {"join", "poll", "signal", "leave"}:\n        return {"status": 400, "body": {"error": "Unknown broker action"}}\n    command = {\n        "action": action,\n        "room": str(body.get("room", "")),\n        "peer": str(body.get("peer", "")),\n        "since": int(body.get("since", 0) or 0),\n    }\n    for key in ("name", "to", "signal"):\n        if key in body:\n            command[key] = body[key]\n    return {"broker": command}\n`,
    "mojojojo.json": `{"name":"Streaming Snake","runtime":"app","entrypoint":"index.html","functions":{"broker":{"language":"python","entrypoint":"functions/broker.py","timeout_ms":3000,"capabilities":["room-broker"]}}}`
  },
  "png-webp": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>PNG to WebP</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>IMAGE TOOL</p><h1>PNG → WebP</h1><p>Convert in your browser. The image never leaves your device.</p><label class="drop">Choose or drop a PNG / JPEG<input id="file" type="file" accept="image/png,image/jpeg" hidden></label><div class="row"><label>Quality <input id="quality" type="range" min="20" max="100" value="82"></label><span id="size"></span></div><div id="result" class="result">Ready.</div></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `const input=document.querySelector('#file'),result=document.querySelector('#result'),size=document.querySelector('#size');input.onchange=async()=>{const file=input.files[0];if(!file)return;const image=await createImageBitmap(file);const canvas=document.createElement('canvas');canvas.width=image.width;canvas.height=image.height;canvas.getContext('2d').drawImage(image,0,0);canvas.toBlob(blob=>{const a=document.createElement('a');a.href=URL.createObjectURL(blob);a.download=file.name.replace(/\\.[^.]+$/,'.webp');a.textContent='Download '+a.download;result.replaceChildren(a);size.textContent=(file.size/1024).toFixed(0)+' KB → '+(blob.size/1024).toFixed(0)+' KB';},'image/webp',Number(document.querySelector('#quality').value)/100)};`,
    "mojojojo.json": `{"name":"PNG to WebP","runtime":"app","entrypoint":"index.html","functions":{}}`
  },
  "json-csv": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>JSON to CSV</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>DATA TOOL</p><h1>JSON → CSV</h1><textarea id="input">[{"name":"Ada","language":"Mojo"},{"name":"Grace","language":"Python"}]</textarea><div class="row"><button id="convert">Convert locally</button><button id="server">Use Python backend</button></div><pre id="result" class="result">Ready.</pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `const input=document.querySelector('#input'),out=document.querySelector('#result');function csv(rows){const keys=[...new Set(rows.flatMap(Object.keys))];const q=v=>'"'+String(v??'').replaceAll('"','""')+'"';return [keys.map(q),...rows.map(r=>keys.map(k=>q(r[k])))].map(r=>r.join(',')).join('\\n')}document.querySelector('#convert').onclick=()=>{try{out.textContent=csv(JSON.parse(input.value))}catch(e){out.textContent=e.message}};document.querySelector('#server').onclick=async()=>{try{out.textContent=await window.mojojojo.invoke('convert',{rows:JSON.parse(input.value)})}catch(e){out.textContent=e.message}};`,
    "functions/convert.py": `import csv, io\n\ndef handler(request):\n    rows = request.get("body", {}).get("rows", [])\n    if not isinstance(rows, list) or not rows:\n        return {"status": 400, "body": "Send a non-empty rows array"}\n    keys = list(dict.fromkeys(key for row in rows for key in row))\n    output = io.StringIO()\n    writer = csv.DictWriter(output, fieldnames=keys)\n    writer.writeheader(); writer.writerows(rows)\n    return {"headers": {"Content-Type": "text/csv; charset=utf-8"}, "body": output.getvalue()}\n`,
    "mojojojo.json": `{"name":"JSON to CSV","runtime":"app","entrypoint":"index.html","functions":{"convert":{"language":"python","entrypoint":"functions/convert.py","timeout_ms":3000}}}`
  },
  "markdown": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Markdown cleaner</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>TEXT TOOL</p><h1>Markdown cleaner</h1><textarea id="input"># A small document\n\nThis is **portable** text with [a link](https://example.com).</textarea><button id="clean">Clean with Python</button><pre id="result" class="result">Ready.</pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `document.querySelector('#clean').onclick=async()=>{const out=document.querySelector('#result');try{out.textContent=await window.mojojojo.invoke('clean',{text:document.querySelector('#input').value})}catch(e){out.textContent=e.message}};`,
    "functions/clean.py": `import re\n\ndef handler(request):\n    text = str(request.get("body", {}).get("text", ""))\n    text = re.sub(r"!\\[([^]]*)\\]\\([^)]*\\)", r"\\1", text)\n    text = re.sub(r"\\[([^]]+)\\]\\([^)]*\\)", r"\\1", text)\n    text = re.sub(r"^[#>]+\\s*", "", text, flags=re.MULTILINE)\n    text = re.sub(r"[*_~\\x60]", "", text)\n    return {"headers":{"Content-Type":"text/plain; charset=utf-8"}, "body": text.strip()}\n`,
    "mojojojo.json": `{"name":"Markdown cleaner","runtime":"app","entrypoint":"index.html","functions":{"clean":{"language":"python","entrypoint":"functions/clean.py","timeout_ms":3000}}}`
  },
  "text-case": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Text case converter</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>TEXT UTILITY</p><h1>Case + slug converter</h1><textarea id="input">Mojo makes fast tools pleasantly small.</textarea><div class="row"><button data-mode="upper">UPPER</button><button data-mode="lower">lower</button><button data-mode="title">Title Case</button><button data-mode="slug">url-slug</button></div><pre id="result" class="result">Choose a conversion.</pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `const input=document.querySelector('#input'),out=document.querySelector('#result');document.querySelectorAll('[data-mode]').forEach(button=>button.onclick=()=>{const text=input.value,mode=button.dataset.mode;out.textContent=mode==='upper'?text.toUpperCase():mode==='lower'?text.toLowerCase():mode==='title'?text.toLowerCase().replace(/\\b\\w/g,c=>c.toUpperCase()):text.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'')});`,
    "mojojojo.json": `{"name":"Text case and slug","runtime":"app","entrypoint":"index.html","functions":{}}`
  },
  "base64": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Base64 and UTF-8</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>DEVELOPER TOOL</p><h1>Base64 + UTF-8</h1><textarea id="input">Hello, 世界 👋</textarea><div class="row"><button id="encode">Encode</button><button id="decode">Decode</button></div><pre id="result" class="result">Ready.</pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `const input=document.querySelector('#input'),out=document.querySelector('#result');document.querySelector('#encode').onclick=()=>{const bytes=new TextEncoder().encode(input.value);let binary='';bytes.forEach(byte=>binary+=String.fromCharCode(byte));out.textContent=btoa(binary)};document.querySelector('#decode').onclick=()=>{try{const binary=atob(input.value.trim()),bytes=Uint8Array.from(binary,char=>char.charCodeAt(0));out.textContent=new TextDecoder().decode(bytes)}catch(error){out.textContent='Invalid Base64: '+error.message}};`,
    "mojojojo.json": `{"name":"Base64 and UTF-8","runtime":"app","entrypoint":"index.html","functions":{}}`
  },
  "color": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Color converter</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>CSS TOOL</p><h1>HEX → RGB → HSL</h1><input id="color" type="color" value="#98d8aa"><div id="swatch" class="result"></div><pre id="result" class="result"></pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS + `#color{width:100%;height:72px;border:0;background:transparent}#swatch{height:90px}`,
    "app.js": `const input=document.querySelector('#color'),swatch=document.querySelector('#swatch'),out=document.querySelector('#result');function update(){const hex=input.value,r=parseInt(hex.slice(1,3),16),g=parseInt(hex.slice(3,5),16),b=parseInt(hex.slice(5,7),16),max=Math.max(r,g,b)/255,min=Math.min(r,g,b)/255,l=(max+min)/2,d=max-min;let h=0,s=0;if(d){s=d/(1-Math.abs(2*l-1));h=max===r/255?60*(((g-b)/255/d)%6):max===g/255?60*((b-r)/255/d+2):60*((r-g)/255/d+4)}if(h<0)h+=360;swatch.style.background=hex;out.textContent=hex.toUpperCase()+'\\nrgb('+r+', '+g+', '+b+')\\nhsl('+Math.round(h)+', '+Math.round(s*100)+'%, '+Math.round(l*100)+'%)'}input.oninput=update;update();`,
    "mojojojo.json": `{"name":"Color converter","runtime":"app","entrypoint":"index.html","functions":{}}`
  },
  "mojo-text": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Mojo backend</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><p>MOJO FUNCTION</p><h1>A frontend with a Mojo backend</h1><textarea id="input">Mojo functions deploy beside ordinary HTML, CSS and JavaScript.</textarea><button id="run">Call Mojo</button><pre id="result" class="result">Ready.</pre></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS,
    "app.js": `document.querySelector('#run').onclick=async()=>{const out=document.querySelector('#result');try{out.textContent=await window.mojojojo.invoke('hello',{text:document.querySelector('#input').value})}catch(e){out.textContent=e.message}};`,
    "functions/hello.mojo": `def main():\n    print(\"{\\\"body\\\":\\\"Hello from a compiled Mojo serverless function.\\\"}\")\n`,
    "mojojojo.json": `{"name":"Mojo backend starter","runtime":"app","entrypoint":"index.html","functions":{"hello":{"language":"mojo","entrypoint":"functions/hello.mojo","timeout_ms":5000}}}`
  },
  "blank": {
    "index.html": `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>My app</title><link rel="stylesheet" href="app.css"></head><body><main class="card"><h1>Build something fast.</h1><p>Edit the files, preview instantly, then publish.</p></main><script src="app.js"></script></body></html>`,
    "app.css": commonCSS, "app.js": `console.log('Hello from mojojojo studio')`,
    "mojojojo.json": `{"name":"My app","runtime":"app","entrypoint":"index.html","functions":{}}`
  }
};

function toast(message) { const node=$("studio-toast"); node.textContent=message; node.hidden=false; clearTimeout(node.timer); node.timer=setTimeout(()=>node.hidden=true,2600); }
function templateName(){const query=new URLSearchParams(location.search),example=query.get("example");return example?`example:${example}`:query.get("template")||$("template-picker").value}
function prettyHTML(source) {
  const tokens=source.trim().replace(/>\s+</g,"><").match(/<!doctype[^>]*>|<!--[\s\S]*?-->|<([a-z][\w-]*)(?:\s[^>]*)?>[^<]*<\/\1>|<\/?[^>]+>|[^<]+/gi)||[];
  const voids=new Set(["area","base","br","col","embed","hr","img","input","link","meta","param","source","track","wbr"]),lines=[];let depth=0;
  for(const token of tokens){const value=token.trim();if(!value)continue;const closing=/^<\//.test(value);if(closing)depth=Math.max(0,depth-1);lines.push("  ".repeat(depth)+value);const open=value.match(/^<([a-z][\w-]*)\b/i),complete=value.match(/^<([a-z][\w-]*)\b[^>]*>[\s\S]*<\/\1>$/i);if(open&&!closing&&!complete&&!voids.has(open[1].toLowerCase())&&!/\/>$/.test(value))depth++}
  return lines.join("\n")+"\n";
}
function formattedStarter(name) {
  const next = structuredClone(templates[name] || templates.blank);
  if (next["index.html"]) next["index.html"] = prettyHTML(next["index.html"]);
  if (next["app.css"]) next["app.css"] = next["app.css"].replace(/\{/g, " {\n  ").replace(/;/g, ";\n  ").replace(/\}/g, "\n}\n\n").trim() + "\n";
  if (name !== "snake" && next["app.js"]) next["app.js"] = next["app.js"].replace(/;/g, ";\n").trim() + "\n";
  return next;
}
function snapshotKey(value=files){return JSON.stringify(Object.keys(value).sort().map(name=>[name,value[name]]))}
function draftKey(id=projectID){return DRAFT_PREFIX+(id||(shareToken&&`share:${shareToken}`)||`new:${templateName()}`)}
function persistDraft(){clearTimeout(draftTimer);try{localStorage.setItem(draftKey(),JSON.stringify({files,activePath,revision:cloudRevision,updated:Date.now()}))}catch{}}
function scheduleDraft(){clearTimeout(draftTimer);draftTimer=setTimeout(persistDraft,DRAFT_DEBOUNCE_MS)}
function recoverDraft(id,revision=""){try{const saved=JSON.parse(localStorage.getItem(draftKey(id))||"null");if(saved?.files&&(!id||saved.revision===revision)){files=saved.files;activePath=files[saved.activePath]?saved.activePath:(files["index.html"]?"index.html":Object.keys(files)[0]);return true}}catch{}return false}
async function exampleStarter(name){
  const slug=name.slice("example:".length);if(!/^[a-z0-9-]+$/.test(slug))throw new Error("Invalid example name");
  const response=await fetch(`/examples/source/${slug}/studio.json`);if(!response.ok)throw new Error("Example is unavailable");
  const manifest=await response.json(),names=manifest.files;if(!Array.isArray(names)||!names.length)throw new Error("Example has no Studio files");
  const entries=await Promise.all(names.map(async path=>{if(typeof path!=="string"||path.includes("..")||path.startsWith("/")||path.length>180)throw new Error("Example contains an unsafe path");const file=await fetch(`/examples/source/${slug}/${path}`);if(!file.ok)throw new Error(`Could not load ${path}`);return [path,await file.text()]}));
  return Object.fromEntries(entries);
}
async function loadTemplate(name=templateName(),recover=false) { files=name.startsWith("example:")?await exampleStarter(name):formattedStarter(name);activePath="index.html";cloudRevision="";mainlineRevision="";viewingVersion="";shareToken="";readOnlyShare=false;lastSavedSnapshot="";lastPublishedSnapshot="";$("open-live").hidden=true;if(recover)recoverDraft("");renderFiles();updateEditor();refreshPreview();refreshFunctions();dirty("browser draft",false); }
function dirty(label="unsaved changes",schedule=true) {
  if(viewingVersion){viewingVersion="";cloudRevision=mainlineRevision;$("version-picker").value="";setProjectURL()}
  $("save-state").textContent=readOnlyShare?"shared copy · unsaved":label;changeSerial++;scheduleDraft();const button=$("save-project");button.textContent=readOnlyShare?"Save a copy *":"Save *";button.setAttribute("aria-label","Save unsaved changes");
  if(schedule&&signedIn&&projectID&&!readOnlyShare){clearTimeout(saveTimer);saveTimer=setTimeout(()=>saveProject(false).catch(error=>setSaveError(error)),SAVE_DEBOUNCE_MS)}
}
function setSaveError(error){$("save-state").textContent="save failed";$("save-project").textContent="Retry save";toast(error.message)}
function fileExtension(name) { return name.split(".").pop().toLowerCase(); }
function editorMode(name) {
  const ext = fileExtension(name);
  if (ext === "html" || ext === "htm") return "htmlmixed";
  if (ext === "css") return "css";
  if (ext === "js" || ext === "mjs") return "javascript";
  if (ext === "json") return { name: "javascript", json: true };
  if (ext === "py" || ext === "mojo") return "python";
  return null;
}
function renderFiles(){
  const tree=$("file-tree");tree.replaceChildren();const renderedFolders=new Set();
  Object.keys(files).sort().forEach(name=>{
    const parts=name.split("/"),depth=parts.length-1;
    for(let index=0;index<depth;index++){
      const folder=parts.slice(0,index+1).join("/");
      if(renderedFolders.has(folder))continue;renderedFolders.add(folder);
      const row=document.createElement("div");row.className="file-folder";row.style.paddingLeft=`${8+index*12}px`;row.textContent=parts[index];tree.append(row);
    }
    const button=document.createElement("button");button.className=name===activePath?"active":"";button.title=name;button.style.paddingLeft=`${8+depth*12}px`;
    const dot=document.createElement("span");dot.className=`file-dot ${fileExtension(name)}`;const label=document.createElement("span");label.className="file-name";label.textContent=parts.at(-1);button.append(dot,label);
    button.onclick=()=>{syncEditor();activePath=name;renderFiles();updateEditor()};tree.append(button);
  });
}
function updateEditor(){
  editorUpdating=true;editor.setOption("mode",editorMode(activePath));editor.setValue(files[activePath]??"");editor.clearHistory();editor.save();editorUpdating=false;
  $("active-path").textContent=activePath;$("language").textContent=fileExtension(activePath).toUpperCase();updateCursor();requestAnimationFrame(()=>editor.refresh());
}
function syncEditor(){if(activePath&&editor){files[activePath]=editor.getValue();editor.save()}}
function updateCursor(){if(!editor)return;const cursor=editor.getCursor();$("cursor").textContent=`Ln ${cursor.line+1}, Col ${cursor.ch+1}`}
function manifest(){try{return JSON.parse(files["mojojojo.json"]||"{}") }catch{return {}}}
function refreshFunctions(){const select=$("function-name");select.replaceChildren();Object.keys(manifest().functions||{}).forEach(name=>select.add(new Option(name,name)));$("run-function").disabled=!select.options.length}
function previewDocument(){syncEditor();let html=files["index.html"]||"<h1>Add index.html</h1>";html=html.replace(/<link[^>]+href=["'](?:\.\/)?app\.css["'][^>]*>/i,`<style>${files["app.css"]||""}</style>`).replace(/<script[^>]+src=["'](?:\.\/)?app\.js["'][^>]*><\/script>/i,`<script>${files["app.js"]||""}<\/script>`);const bridge=`<script>window.mojojojo={hostURL:${JSON.stringify(location.href)},canPublish:${signedIn},invoke:(name,body)=>new Promise((resolve,reject)=>{const id=Math.random().toString(36).slice(2);addEventListener('message',function done(e){if(e.data?.type==='mj-result'&&e.data.id===id){removeEventListener('message',done);e.data.ok?resolve(e.data.body):reject(new Error(e.data.error))}});parent.postMessage({type:'mj-invoke',id,name,body},'*')}),invite:room=>new Promise((resolve,reject)=>{const id=Math.random().toString(36).slice(2);addEventListener('message',function done(e){if(e.data?.type==='mj-invite-result'&&e.data.id===id){removeEventListener('message',done);e.data.ok?resolve(e.data.url):reject(new Error(e.data.error))}});parent.postMessage({type:'mj-invite',id,room},'*')})};<\/script>`;return html.replace(/<head([^>]*)>/i,`<head$1>${bridge}`)}
function refreshPreview(){clearTimeout(previewTimer);previewTimer=setTimeout(()=>{$("preview").srcdoc=previewDocument();refreshFunctions()},180)}

async function api(path,options={}){const response=await fetch(path,{...options,headers:{"Content-Type":"application/json",...(options.headers||{})}});let data;try{data=await response.json()}catch{data={error:await response.text()}}if(!response.ok){const error=new Error(data.error||`Request failed (${response.status})`);error.status=response.status;error.data=data;throw error}return data}
async function loadProjects(){if(!signedIn)return;const data=await api("/v1/projects");const picker=$("project-picker");picker.replaceChildren(new Option("New project",""));for(const p of data.projects){const option=new Option(p.name,p.id);option.dataset.url=p.url||"";picker.add(option)}}
function setProjectURL(){const url=new URL("/studio",location.origin);if(readOnlyShare&&shareToken)url.searchParams.set("share",shareToken);else if(projectID)url.searchParams.set("project",projectID);else return;if(viewingVersion)url.searchParams.set("version",viewingVersion);history.replaceState(null,"",url)}
async function loadVersions(){
  const picker=$("version-picker"),share=$("share-project");picker.replaceChildren(new Option("Mainline",""));
  if(!signedIn||!projectID||readOnlyShare){picker.hidden=true;share.hidden=true;return}
  const data=await api(`/v1/projects/${projectID}/versions`);for(const version of data.versions){const when=new Date(version.created_at);picker.add(new Option(`Saved ${when.toLocaleString()}`,version.id))}
  picker.hidden=false;share.hidden=false;picker.value=viewingVersion;
}
async function ensureProject(){
  if(projectID&&!readOnlyShare)return projectID;
  if(!signedIn){persistDraft();const next=location.pathname+location.search;location.href=`/auth/login?next=${encodeURIComponent(next)}`;throw new Error("Sign in to save")}
  const oldKey=draftKey(),m=manifest(),name=m.name||prompt("Project name","My app")||"My app",request={name,runtime:"app",category:new URLSearchParams(location.search).get("kind")||"tool",description:"Built in mojojojo studio"};let data;
  try{data=await api("/v1/projects",{method:"POST",body:JSON.stringify(request)})}catch(error){if(error.status!==409)throw error;const base=name.toLowerCase().replace(/[^a-z0-9]+/g,"-").replace(/^-|-$/g,"").slice(0,40)||"app";request.slug=`${base}-${crypto.randomUUID().slice(0,6)}`;data=await api("/v1/projects",{method:"POST",body:JSON.stringify(request)})}
  projectID=data.project.id;cloudRevision="";mainlineRevision="";viewingVersion="";shareToken="";readOnlyShare=false;lastSavedSnapshot="";try{localStorage.setItem(draftKey(),localStorage.getItem(oldKey)||"");localStorage.removeItem(oldKey)}catch{}await loadProjects();$("project-picker").value=projectID;setProjectURL();return projectID
}
async function saveProject(announce=true){
  clearTimeout(saveTimer);if(savePromise)return savePromise;syncEditor();const serial=changeSerial,snapshot=structuredClone(files),fingerprint=snapshotKey(snapshot);
  if(projectID&&!readOnlyShare&&fingerprint===lastSavedSnapshot){$("save-state").textContent=viewingVersion?"saved version":"saved";$("save-project").textContent="Save";if(announce)toast("No changes to save");return projectID}
  $("save-state").textContent="saving";$("save-project").textContent="Saving…";$("save-project").disabled=true;
  savePromise=(async()=>{const id=await ensureProject();const data=await api(`/v1/projects/${id}/files`,{method:"PUT",body:JSON.stringify({files:snapshot,revision:cloudRevision})});cloudRevision=data.revision||cloudRevision;mainlineRevision=cloudRevision;lastSavedSnapshot=fingerprint;viewingVersion="";$("save-state").textContent=data.changed===false?"already saved":"saved new version";setProjectURL();await loadVersions();if(serial===changeSerial){try{localStorage.removeItem(draftKey())}catch{}$("save-project").textContent="Saved ✓";setTimeout(()=>{$("save-project").textContent="Save"},1200)}else{scheduleDraft();$("save-project").textContent="Save *";saveTimer=setTimeout(()=>saveProject(false).catch(error=>setSaveError(error)),SAVE_DEBOUNCE_MS)}if(announce)toast(data.changed===false?"No changes to save":"New version saved");return id})();
  try{return await savePromise}finally{savePromise=null;$("save-project").disabled=false}
}
async function openProject(id,version=""){
  clearTimeout(saveTimer);projectID=id;shareToken="";readOnlyShare=false;viewingVersion=version;cloudRevision="";mainlineRevision="";lastPublishedSnapshot="";$("open-live").hidden=true;if(!id){await loadTemplate(templateName(),true);await loadVersions();return}
  const query=version?`?version=${encodeURIComponent(version)}`:"",data=await api(`/v1/projects/${id}/files${query}`);mainlineRevision=data.mainline_revision||data.revision||"";cloudRevision=mainlineRevision;files=data.files;const savedSnapshot=snapshotKey(files);
  if(!Object.keys(files).length){await loadTemplate();return}activePath=files["index.html"]?"index.html":Object.keys(files)[0];const recovered=!version&&recoverDraft(id,cloudRevision);lastSavedSnapshot=savedSnapshot;renderFiles();updateEditor();refreshPreview();refreshFunctions();$("project-picker").value=id;setProjectURL();await loadVersions();$("save-state").textContent=recovered?"recovered local draft":version?"saved version · edit to restore":"mainline saved";$("save-project").textContent=recovered?"Save *":"Save";if(recovered){dirty("recovered local draft");toast("Recovered unsaved browser changes")}
}
async function openShared(token,version=""){
  clearTimeout(saveTimer);projectID="";shareToken=token;readOnlyShare=true;viewingVersion=version;lastPublishedSnapshot="";$("open-live").hidden=true;const query=version?`?version=${encodeURIComponent(version)}`:"",data=await api(`/v1/shared/projects/${encodeURIComponent(token)}/files${query}`);cloudRevision=data.mainline_revision||data.revision||"";mainlineRevision=cloudRevision;files=data.files;lastSavedSnapshot=snapshotKey(files);activePath=files["index.html"]?"index.html":Object.keys(files)[0];renderFiles();updateEditor();refreshPreview();refreshFunctions();$("project-picker").value="";$("version-picker").hidden=true;$("share-project").hidden=true;$("save-state").textContent=version?"shared saved version":"shared mainline";$("save-project").textContent="Save a copy";history.replaceState(null,"",location.pathname+location.search)
}
async function shareProject(){
  try{const id=await saveProject(false),data=await api(`/v1/projects/${id}/share`,{method:"POST",body:"{}"}),version=$("version-picker").value,url=new URL("/studio",location.origin);url.searchParams.set("share",data.share_token);if(version)url.searchParams.set("version",version);if(navigator.clipboard)await navigator.clipboard.writeText(url);else prompt("Copy share link",url);toast(version?"Saved version link copied":"Mainline link copied")}catch(error){setSaveError(error)}
}
async function downloadProject(){try{const id=await saveProject(false),link=document.createElement("a");link.href=`/v1/projects/${id}/download`;link.download="";document.body.append(link);link.click();link.remove();toast("Project ZIP ready")}catch(error){setSaveError(error)}}

async function publishCurrent(announce=true){
  const fingerprint=snapshotKey();if(!$("open-live").hidden&&lastPublishedSnapshot===fingerprint)return $("open-live").href;
  const id=await saveProject(false),data=await api(`/v1/projects/${id}/publish`,{method:"POST",body:"{}"});$("open-live").href=data.deployment.url;$("open-live").hidden=false;lastPublishedSnapshot=fingerprint;if(announce)toast("Published live");return data.deployment.url
}

window.addEventListener("message",async event=>{
  if(event.source!==$("preview").contentWindow)return;
  if(event.data?.type==="mj-invite"){
    const reply={type:"mj-invite-result",id:event.data.id,ok:false};try{const live=await publishCurrent(false),url=new URL(live,location.origin),room=String(event.data.room||"").toUpperCase().replace(/[^A-Z0-9]/g,"").slice(0,12);url.searchParams.set("room",room);reply.url=url.href;reply.ok=true;toast("Live room invite ready")}catch(error){reply.error=error.message;toast(error.message)}event.source.postMessage(reply,"*");return
  }
  if(event.data?.type!=="mj-invoke")return;
  const reply={type:"mj-result",id:event.data.id,ok:false};try{const id=await saveProject(false);const response=await fetch(`/v1/projects/${id}/preview/functions/${encodeURIComponent(event.data.name)}`,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(event.data.body)});const type=response.headers.get("content-type")||"";reply.body=type.includes("json")?await response.json():await response.text();reply.ok=response.ok;if(!response.ok)reply.error=reply.body?.error||String(reply.body)}catch(error){reply.error=error.message}event.source.postMessage(reply,"*")
});

async function runFunction(){const name=$("function-name").value;if(!name)return;try{const id=await saveProject(),body=JSON.parse($("function-input").value||"{}");const started=performance.now(),response=await fetch(`/v1/projects/${id}/preview/functions/${encodeURIComponent(name)}`,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)}),text=await response.text();$("function-output").textContent=`HTTP ${response.status} · ${Math.round(performance.now()-started)}ms\n\n${text}`}catch(error){$("function-output").textContent=error.message}}
function setAgentOpen(open){$("agent-sidebar").hidden=!open;$("toggle-agent").setAttribute("aria-expanded",String(open));if(open)setTimeout(()=>$("agent-prompt").focus(),0)}
function agentMessage(kind,text){$("agent-output").querySelector(".agent-empty")?.remove();const node=document.createElement("div");node.className=`agent-message ${kind}`;node.textContent=text;$("agent-output").append(node);$("agent-output").scrollTop=$("agent-output").scrollHeight;return node}
function agentEvent(step,state){
  const kind=step.kind||state.event;
  if(kind==="run"){agentRunID=step.id||step.meta?.run_id||agentRunID;agentSession=step.session||step.meta?.session||agentSession;return}
  if(kind==="text"){agentText+=step.text||"";if(!state.answer)state.answer=agentMessage("assistant","");state.answer.textContent=agentText;return}
  if(kind==="reasoning"){if(step.text?.trim())agentMessage("reasoning",step.text.trim());return}
  if(kind==="tool_call"){const detail=step.name||step.meta?.name||"tool";agentMessage("tool",`↳ ${detail}`);return}
  if(kind==="tool_result"){const detail=(step.text||"").trim().split("\n")[0];if(detail)agentMessage("tool",`✓ ${detail.slice(0,180)}`);return}
  if(kind==="usage"){
    const cost=step.meta?.cost,credits=cost?.credits??0,balance=cost?.balance;const suffix=balance==null?"":` · ${balance} left`;
    agentMessage("usage",`${step.text||"Run complete"} · ${credits} credit${credits===1?"":"s"}${suffix}`);$("agent-status").textContent=`Complete · ${credits} credits`;return;
  }
  if(kind==="error")agentMessage("error",step.text||"Agent run failed");
}
async function askAgent(){
  if(!signedIn){location.href="/auth/login?next=/studio";return}syncEditor();const instruction=$("agent-prompt").value.trim();if(!instruction)return;
  const context=Object.entries(files).sort(([a],[b])=>a.localeCompare(b)).map(([name,content])=>`<file path=${JSON.stringify(name)}>\n${content}\n</file>`).join("\n\n");
  if(context.length>220000){agentMessage("error","This draft is too large for one agent turn. Remove generated assets or ask about fewer source files.");return}
  const prompt=`${instruction}\n\nYou are editing a mojojojo Studio app. The browser workspace embedded below is authoritative; do not assume the server filesystem contains it. Diagnose before changing code. For every file you change or create, the final answer MUST include its complete replacement as <file path="relative/name">content</file>. Do not omit unchanged portions and do not return a patch.\n\n${context}`;
  setAgentOpen(true);agentMessage("user",instruction);$("agent-prompt").value="";$("ask-agent").disabled=true;$("stop-agent").hidden=false;$("agent-status").textContent="Working…";agentText="";agentRunID="";const state={event:"",answer:null};
  try{
    const body={prompt,model:$("agent-model").value,effort:$("agent-effort").value};if(agentSession)body.session=agentSession;
    const response=await fetch("/v1/agent/runs",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)});agentRunID=response.headers.get("X-Run-ID")||"";
    if(!response.ok){let payload={};try{payload=await response.json()}catch{}throw new Error(payload.error||`Agent failed (${response.status})`)}
    const reader=response.body.getReader(),decoder=new TextDecoder();let buffer="";
    while(true){const {value,done}=await reader.read();buffer+=decoder.decode(value||new Uint8Array(),{stream:!done}).replaceAll("\r\n","\n");let at;while((at=buffer.indexOf("\n\n"))>=0){const block=buffer.slice(0,at);buffer=buffer.slice(at+2);state.event=block.split("\n").find(line=>line.startsWith("event:"))?.slice(6).trim()||"";const raw=block.split("\n").filter(line=>line.startsWith("data:")).map(line=>line.slice(5).trimStart()).join("\n");if(raw)try{agentEvent(JSON.parse(raw),state)}catch{}}if(done)break}
    $("apply-agent").disabled=!/<file\s+path=["']/.test(agentText);if(!agentText)$("agent-status").textContent="Complete";
  }catch(error){agentMessage("error",error.message);$("agent-status").textContent="Failed"}finally{$("ask-agent").disabled=false;$("stop-agent").hidden=true;agentRunID=""}
}
async function stopAgent(){if(!agentRunID)return;$("stop-agent").disabled=true;try{await fetch(`/v1/agent/runs/${encodeURIComponent(agentRunID)}/interrupt`,{method:"POST"});$("agent-status").textContent="Stopping…"}finally{$("stop-agent").disabled=false}}
function applyAgent(){const pattern=/<file\s+path=["']([^"']+)["']>([\s\S]*?)<\/file>/g;let count=0,firstChanged="",match;while((match=pattern.exec(agentText))){const name=match[1];if(name&&!name.includes("..")&&!name.startsWith("/")&&name.length<=180){files[name]=match[2].replace(/^\n|\n$/g,"");firstChanged=firstChanged||name;count++}}if(count){activePath=firstChanged;renderFiles();updateEditor();refreshPreview();dirty(`${count} agent file${count===1?"":"s"} applied`);$("apply-agent").disabled=true;toast(`${count} agent change${count===1?"":"s"} applied`)}}

editor=window.CodeMirror.fromTextArea($("source"),{
  mode:"htmlmixed",theme:"base16-dark",lineNumbers:true,lineWrapping:false,
  indentUnit:2,tabSize:2,indentWithTabs:false,autoCloseBrackets:true,
  matchBrackets:true,styleActiveLine:true,
  extraKeys:{
    Tab(cm){if(cm.somethingSelected())cm.indentSelection("add");else cm.replaceSelection("  ","end")},
    "Shift-Tab"(cm){cm.indentSelection("subtract")},
    "Ctrl-S"(){saveProject().catch(error=>toast(error.message))},
    "Cmd-S"(){saveProject().catch(error=>toast(error.message))},
  },
});
editor.getInputField().setAttribute("aria-label","Source code editor");
editor.on("change",()=>{editor.save();if(editorUpdating)return;syncEditor();dirty();refreshPreview()});
editor.on("cursorActivity",updateCursor);
$("load-template").onclick=()=>loadTemplate($("template-picker").value).catch(setSaveError);$("new-project").onclick=()=>{clearTimeout(saveTimer);projectID="";cloudRevision="";mainlineRevision="";viewingVersion="";shareToken="";readOnlyShare=false;agentSession="";$("project-picker").value="";loadTemplate().catch(setSaveError);history.replaceState(null,"","/studio")};$("project-picker").onchange=e=>openProject(e.target.value).catch(setSaveError);$("version-picker").onchange=e=>{const next=e.target.value;if(snapshotKey()!==lastSavedSnapshot&&!confirm("Open another version and keep this draft only in your browser?")){e.target.value=viewingVersion;return}openProject(projectID,next).catch(setSaveError)};$("save-project").onclick=()=>saveProject().catch(setSaveError);$("share-project").onclick=shareProject;$("download-project").onclick=downloadProject;
$("publish-project").onclick=()=>publishCurrent().catch(error=>toast(error.message));
$("submit-project").onclick=async()=>{try{const id=await saveProject(),data=await api(`/v1/projects/${id}/submit`,{method:"POST",body:"{}"});toast(data.submission.status==="approved"?"Published to showcase":"Submitted for review")}catch(e){toast(e.message)}};
$("run-function").onclick=runFunction;$("ask-agent").onclick=askAgent;$("apply-agent").onclick=applyAgent;$("stop-agent").onclick=stopAgent;$("toggle-agent").onclick=()=>setAgentOpen($("agent-sidebar").hidden);$("close-agent").onclick=()=>setAgentOpen(false);
$("add-file").onclick=()=>{const name=prompt("File path","functions/hello.py");if(!name||name.includes("..")||name.startsWith("/"))return;syncEditor();files[name]=files[name]||"";activePath=name;renderFiles();updateEditor();dirty()};$("delete-file").onclick=()=>{if(Object.keys(files).length<=1||!confirm(`Delete ${activePath}?`))return;delete files[activePath];activePath=Object.keys(files)[0];renderFiles();updateEditor();refreshPreview();dirty()};
document.querySelectorAll(".preview-tabs button").forEach(button=>button.onclick=()=>{document.querySelectorAll(".preview-tabs button").forEach(x=>x.classList.toggle("active",x===button));document.querySelectorAll(".tab-panel").forEach(x=>x.classList.toggle("active",x.id===`${button.dataset.tab}-panel`))});
addEventListener("beforeunload",()=>{syncEditor();if(snapshotKey()!==lastSavedSnapshot)persistDraft()});

await loadProjects().catch(()=>{});const initial=new URLSearchParams(location.search),shared=initial.get("share"),initialProject=initial.get("project"),initialVersion=initial.get("version")||"";if(shared)await openShared(shared,initialVersion).catch(error=>{setSaveError(error);loadTemplate(templateName(),true).catch(setSaveError)});else if(initialProject&&signedIn)await openProject(initialProject,initialVersion).catch(error=>{setSaveError(error);loadTemplate(templateName(),true).catch(setSaveError)});else await loadTemplate(templateName(),true).catch(setSaveError);
