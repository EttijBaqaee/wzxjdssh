'use strict';
const $=id=>document.getElementById(id);
const state={page:1,total:0,limit:25,view:'overview',loggedIn:false,busy:false,pending:false,autoUpdate:true,lastActivity:Date.now(),lastSignal:0,activityPending:false,idleTimeout:3600000};
let activityTimer=0;
const statusNames={connected:'已连接',authenticating:'认证中',auth_rejected:'认证拒绝',closed:'已关闭',handshake_failed:'握手未完成',rejected:'接入拒绝',timeout:'连接超时',interrupted:'中断'};
const number=v=>new Intl.NumberFormat('zh-CN').format(v??0);
const date=v=>v?new Date(v).toLocaleString('zh-CN',{hour12:false}):'—';
function el(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
async function api(path,options={}){const r=await fetch('api/'+path,{credentials:'same-origin',...options});const idle=Number(r.headers.get('X-Session-Idle-Seconds'));if(idle>0)state.idleTimeout=idle*1000;let data;try{data=await r.json();}catch{throw new Error('服务器响应异常');}if(r.status===401){showLogin();throw new Error(data.error||'登录已过期');}if(!r.ok)throw new Error(data.error||'请求失败');return data;}
function showLogin(){state.loggedIn=false;clearTimeout(activityTimer);$('login-view').hidden=false;$('app-view').hidden=true;if($('detail-dialog').open)$('detail-dialog').close();}
async function enter(){const session=await api('session');$('admin-name').textContent=session.username;$('version').textContent='v'+session.version;$('login-view').hidden=true;$('app-view').hidden=false;state.loggedIn=true;state.lastActivity=Date.now();state.idleTimeout=(session.idle_timeout_seconds||3600)*1000;await sendActivity();await refresh();}
$('login-form').addEventListener('submit',async e=>{e.preventDefault();$('login-error').textContent='';const button=e.currentTarget.querySelector('button');button.disabled=true;try{await api('login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:$('login-name').value,password:$('login-password').value})});$('login-password').value='';await enter();}catch(err){$('login-error').textContent=err.message;}finally{button.disabled=false;}});
$('logout').addEventListener('click',async()=>{try{await api('logout',{method:'POST'});showLogin();}catch(err){$('app-error').textContent=err.message;}});
function rank(id,items){const list=$(id);list.replaceChildren();if(!items.length){list.append(el('li','暂无数据','empty'));return;}items.forEach((item,i)=>{const row=el('li');const name=el('span',item.name||'（空用户名）','rank-name');name.title=item.name;row.append(el('span',String(i+1).padStart(2,'0'),'rank-number'),name,el('span',number(item.count),'rank-count'));list.append(row);});}
function chart(points){
  state.daily=points;
  const ns='http://www.w3.org/2000/svg';const svg=document.createElementNS(ns,'svg');
  const width=Math.max(280,$('trend').clientWidth);const step=(width-54)/points.length;const barWidth=Math.max(1.6,Math.min(10,(step-3)/2));
  svg.setAttribute('viewBox','0 0 '+width+' 200');svg.setAttribute('role','img');svg.setAttribute('aria-label','最近30天连接与认证尝试数量');
  const max=Math.max(1,...points.flatMap(p=>[p.connections,p.attempts]));
  const add=(tag,attrs,text)=>{const n=document.createElementNS(ns,tag);for(const[k,v]of Object.entries(attrs))n.setAttribute(k,String(v));if(text!==undefined)n.textContent=text;svg.append(n);return n;};
  for(let i=0;i<4;i++){const y=20+i*46;add('line',{x1:42,x2:width-10,y1:y,y2:y,class:'chart-grid'});add('text',{x:0,y:y+3,class:'chart-axis'},number(Math.round(max*(1-i/3))));}
  points.forEach((p,i)=>{const x=42+i*step;for(const[field,offset,cls]of[['connections',0,'bar-connection'],['attempts',barWidth+1,'bar-attempt']]){const h=p[field]/max*138;const bar=add('rect',{x:x+offset,y:158-h,width:barWidth,height:Math.max(h,p[field]?2:0),rx:1.5,class:cls});const title=document.createElementNS(ns,'title');title.textContent=p.day+' '+(field==='connections'?'连接':'认证')+' '+p[field];bar.append(title);}if(i%(width<600?10:5)===0||i===points.length-1)add('text',{x:x+step/2,y:185,class:'chart-axis','text-anchor':i===0?'start':i===points.length-1?'end':'middle'},p.day.slice(5));});
  $('trend').replaceChildren(svg);
}
async function overview(){const data=await api('stats');const s=data.stored;const cards=[['累计连接',s.connections],['当前在线',s.active],['认证尝试',s.attempts],['独立来源 IP',s.unique_ips],['今日连接 · UTC',s.today],['接入拒绝',s.rejected]];$('cards').replaceChildren(...cards.map(([label,value])=>{const card=el('div',undefined,'card');card.append(el('div',label,'label'),el('div',number(value),'value'));return card;}));rank('rank-sources',s.sources);rank('rank-users',s.usernames);rank('rank-methods',s.methods);chart(s.daily);renderStrategy(data.runtime.strategy);$('storage-health').textContent='统计范围：当前保留的数据 · 本次进程存储错误：'+number(data.runtime.storage_errors);}
async function connections(){
  const form=new FormData($('filter-form'));const query=new URLSearchParams({page:String(state.page),limit:String(state.limit)});for(const[k,v]of form)if(v)query.set(k,v);
  const data=await api('connections?'+query);state.total=data.total;const pages=Math.max(1,Math.ceil(data.total/state.limit));if(state.page>pages){state.page=pages;return connections();}
  const body=$('connection-rows');body.replaceChildren();
  if(!data.items.length){const row=el('tr');const cell=el('td','暂无连接记录','empty');cell.colSpan=7;row.append(cell);body.append(row);}
  const cell=(label,text)=>{const n=el('td',text);n.dataset.label=label;return n;};
  for(const c of data.items){const row=el('tr');row.append(cell('连接编号','#'+c.id),cell('来源',c.peer),cell('连接时间',date(c.started_at)));const status=cell('状态');status.append(el('span',statusNames[c.status]||c.status,'badge'+(!c.ended_at?' active':'')));row.append(status,cell('认证次数',number(c.auth_attempts)),cell('持续时间',c.ended_at?(c.duration_ms/1000).toFixed(1)+'s':((Date.now()-c.started_at)/1000).toFixed(1)+'s'));const action=cell('操作');const button=el('button','详情','quiet');button.addEventListener('click',()=>detail(c.id));action.append(button);row.append(action);body.append(row);}
  $('record-count').textContent='共 '+number(data.total)+' 条连接';$('page-number').textContent=state.page+' / '+pages;$('previous').disabled=state.page<=1;$('next').disabled=state.page>=pages;
}
async function detail(id){try{const {connection:c,attempts}=await api('connections/'+id);$('detail-title').textContent='#'+c.id+' · '+c.ip;const meta=$('detail-meta');meta.replaceChildren();for(const [label,value] of [['来源地址',c.peer],['连接时间',date(c.started_at)],['结束时间',date(c.ended_at)],['状态',statusNames[c.status]||c.status],['关闭原因',c.reason||'—'],['协议模式',c.mode],['尝试用户名',c.username||'—'],['客户端标识',c.client_version||'—'],['发送 / 接收',number(c.bytes_sent)+' / '+number(c.bytes_received)+' bytes']])meta.append(el('dt',label),el('dd',value));const list=$('detail-attempts');list.replaceChildren();if(!attempts.length)list.append(el('p','此连接未提交认证信息。','empty'));for(const a of attempts){const item=el('article',undefined,'attempt');const head=el('header');head.append(el('span',a.method+' · '+a.result),el('span',date(a.at)));item.append(head,el('p','用户名：'+(a.username||'（空）')));if(a.fingerprint)item.append(el('p','公钥指纹：'+a.fingerprint));item.append(el('p',a.public_key?'公钥正文：':(a.method==='password'?'密码：':'交互回答：')+(a.encoding==='base64'?'（Base64 原始字节）':''),'credential-label'),el('pre',a.public_key||a.credential||(a.method==='password'?'（空密码）':'（无内容）'),'credential-value'));if(a.truncated)item.append(el('p','记录超出字段上限，内容已截断。','muted'));list.append(item);}if(!$('detail-dialog').open)$('detail-dialog').showModal();}catch(err){$('app-error').textContent=err.message;}}
$('close-detail').addEventListener('click',()=>$('detail-dialog').close());
async function refresh(){if(!state.loggedIn)return;if(state.busy){state.pending=true;return;}state.busy=true;try{$('app-error').textContent='';if(state.view==='overview')await overview();else if(state.view==='connections')await connections();else await loadSettings();$('updated').textContent='更新于 '+new Date().toLocaleTimeString('zh-CN',{hour12:false});}catch(err){$('app-error').textContent=err.message;}finally{state.busy=false;if(state.pending){state.pending=false;refresh();}}}
function tab(view){state.view=view;$('overview').hidden=view!=='overview';$('connections-view').hidden=view!=='connections';$('settings-view').hidden=view!=='settings';for(const v of ['overview','connections','settings']){$('tab-'+v).classList.toggle('active',v===view);$('tab-'+v).setAttribute('aria-selected',String(v===view));}refresh();}
$('tab-overview').addEventListener('click',()=>tab('overview'));$('tab-connections').addEventListener('click',()=>tab('connections'));$('refresh').addEventListener('click',refresh);
$('filter-form').addEventListener('submit',e=>{e.preventDefault();state.page=1;refresh();});$('filter-form').addEventListener('reset',()=>{state.page=1;setTimeout(refresh,0);});$('previous').addEventListener('click',()=>{state.page--;refresh();});$('next').addEventListener('click',()=>{state.page++;refresh();});
setInterval(()=>{if(state.autoUpdate&&state.view!=='settings'&&!document.hidden&&!$('detail-dialog').open)refresh();},10000);
enter().catch(()=>showLogin());

const activityKey='wzxjdssh-last-activity:'+location.pathname;
let lastBroadcast=0;
async function sendActivity(){
  if(!state.loggedIn||state.activityPending)return;
  state.activityPending=true;state.lastSignal=Date.now();
  try{await api('activity',{method:'POST'});}catch(err){if(state.loggedIn)$('app-error').textContent=err.message;}finally{state.activityPending=false;}
}
function checkIdle(){
  if(!state.loggedIn||Date.now()-state.lastActivity<state.idleTimeout)return false;
  showLogin();$('login-error').textContent='闲置超时，请重新登录。';
  fetch('api/logout',{method:'POST',credentials:'same-origin'}).catch(()=>{});return true;
}
function userActivity(event){
  if(!event.isTrusted||!state.loggedIn||checkIdle())return;
  const now=Date.now();state.lastActivity=now;
  if(now-lastBroadcast>1000){lastBroadcast=now;try{localStorage.setItem(activityKey,String(now));}catch{}}
  clearTimeout(activityTimer);
  if(now-state.lastSignal>=30000)sendActivity();else activityTimer=setTimeout(sendActivity,1000);
}
for(const name of ['pointerdown','pointermove','keydown','wheel','touchstart','input'])document.addEventListener(name,userActivity,{passive:true});
addEventListener('focus',userActivity);
document.addEventListener('visibilitychange',()=>{if(!document.hidden)checkIdle();});
addEventListener('storage',event=>{if(event.key===activityKey){const at=Number(event.newValue);if(at>state.lastActivity&&at<=Date.now()+5000)state.lastActivity=at;}});
setInterval(checkIdle,5000);
function renderAutoUpdate(){
  $('auto-refresh').textContent='自动更新：'+(state.autoUpdate?'开':'关');
  $('auto-refresh').setAttribute('aria-pressed',String(state.autoUpdate));
  $('auto-refresh').title=state.autoUpdate?'每 10 秒自动更新，点击暂停':'点击开启每 10 秒自动更新';
  document.querySelector('.live-dot').classList.toggle('paused',!state.autoUpdate);
}
try{state.autoUpdate=localStorage.getItem('wzxjdssh-auto-update')!=='false';}catch{}
renderAutoUpdate();
function durationLabel(ms){if(ms>=3600000&&ms%3600000===0)return number(ms/3600000)+' 小时';if(ms>=60000&&ms%60000===0)return number(ms/60000)+' 分钟';if(ms>=1000)return number(ms/1000)+' 秒';return number(ms)+' 毫秒';}
function renderStrategy(policy){
  const fields=$('strategy-fields');fields.replaceChildren();if(!policy){$('strategy-mode').textContent='暂不可用';return;}
  const banner=policy.mode==='banner';$('strategy-mode').textContent=banner?'慢速横幅':'SSH 认证延迟';
  $('strategy-description').textContent=banner?'首行等待 '+durationLabel(policy.delay_ms)+'，之后以同样间隔发送随机横幅。':'先完成 SSH 握手，接收认证后等待 '+durationLabel(policy.delay_ms)+' 再拒绝认证。';
  const items=[[banner?'首行 / 行间隔':'每次认证延迟',durationLabel(policy.delay_ms)],banner?['横幅最大行长',number(policy.max_line_length)+' 字节']:['单连接认证上限',number(policy.max_auth_tries)+' 次'],['连接最长持续',durationLabel(policy.max_connection_time_ms)],['握手超时',durationLabel(policy.handshake_timeout_ms)],['认证空闲超时',durationLabel(policy.idle_timeout_ms)],['总连接上限',number(policy.max_clients)],['每 IP 并发上限',policy.max_clients_per_ip?number(policy.max_clients_per_ip):'不限制'],['每 IP 每分钟接入',policy.connections_per_minute?number(policy.connections_per_minute):'不限制']];
  for(const[label,value]of items){const pair=el('div');pair.append(el('dt',label),el('dd',value));fields.append(pair);}
}
let resizeTimer=0;addEventListener('resize',()=>{clearTimeout(resizeTimer);resizeTimer=setTimeout(()=>{if(state.loggedIn&&state.view==='overview'&&state.daily)chart(state.daily);},100);});
$('auto-refresh').addEventListener('click',()=>{state.autoUpdate=!state.autoUpdate;try{localStorage.setItem('wzxjdssh-auto-update',String(state.autoUpdate));}catch{};renderAutoUpdate();if(state.autoUpdate)refresh();});
async function loadSettings(){const settings=await api('settings');$('idle-minutes').value=settings.idle_timeout_minutes;state.idleTimeout=settings.idle_timeout_seconds*1000;}
$('tab-settings').addEventListener('click',()=>tab('settings'));
$('settings-form').addEventListener('submit',async event=>{event.preventDefault();const button=event.currentTarget.querySelector('button');const message=$('settings-message');button.disabled=true;message.textContent='';try{const settings=await api('settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({idle_timeout_minutes:Number($('idle-minutes').value)})});state.idleTimeout=settings.idle_timeout_seconds*1000;state.lastActivity=Date.now();$('idle-minutes').value=settings.idle_timeout_minutes;message.className='muted';message.textContent='设置已保存并生效。';try{localStorage.setItem('wzxjdssh-idle-timeout:'+location.pathname,String(state.idleTimeout));}catch{}}catch(err){message.className='error';message.textContent=err.message;}finally{button.disabled=false;}});
addEventListener('storage',event=>{if(event.key==='wzxjdssh-idle-timeout:'+location.pathname){const ms=Number(event.newValue);if(ms>=60000&&ms<=86400000)state.idleTimeout=ms;}});
