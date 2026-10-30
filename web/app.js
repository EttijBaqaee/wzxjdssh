'use strict';
const $=id=>document.getElementById(id);
const state={page:1,total:0,limit:25,view:'overview',loggedIn:false,busy:false,pending:false,autoUpdate:true,lastActivity:Date.now(),lastSignal:0,activityPending:false,idleTimeout:3600000};
let activityTimer=0;
const statusNames={probe_blocked:'探针静默丢弃',connected:'已连接',authenticating:'认证中',auth_rejected:'认证拒绝',closed:'已关闭',handshake_failed:'握手未完成',rejected:'接入拒绝',timeout:'连接超时',capacity_evicted:'容量回收',interrupted:'中断',auth_interrupted:'认证中断',cooldown_auth_interrupted:'冷却重连认证中断',simulated_disconnect:'旧版仿真后断开',simulated_network:'仿真网络迟滞',simulated_closed:'仿真会话结束'};
const number=v=>new Intl.NumberFormat('zh-CN').format(v??0);
const date=v=>v?new Date(v).toLocaleString('zh-CN',{hour12:false}):'—';
function el(tag,text,cls){const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(cls)n.className=cls;return n;}
async function api(path,options={}){const r=await fetch('api/'+path,{credentials:'same-origin',...options});const idle=Number(r.headers.get('X-Session-Idle-Seconds'));if(idle>0)state.idleTimeout=idle*1000;let data;try{data=await r.json();}catch{throw new Error('服务器响应异常');}if(r.status===401){showLogin();throw new Error(data.error||'登录已过期');}if(!r.ok)throw new Error(data.error||'请求失败');return data;}
function showLogin(){state.loggedIn=false;clearTimeout(activityTimer);$('login-view').hidden=false;$('app-view').hidden=true;for(const id of ['detail-dialog','ip-dialog'])if($(id).open)$(id).close();}
async function enter(){const session=await api('session');$('admin-name').textContent=session.username;$('version').textContent='v'+session.version;$('login-view').hidden=true;$('app-view').hidden=false;state.loggedIn=true;state.lastActivity=Date.now();state.idleTimeout=(session.idle_timeout_seconds||3600)*1000;await sendActivity();await refresh();}
$('login-form').addEventListener('submit',async e=>{e.preventDefault();$('login-error').textContent='';const username=$('login-name').value.trim();const password=$('login-password').value.trim();$('login-name').value=username;$('login-password').value=password;const button=e.currentTarget.querySelector('button');button.disabled=true;try{await api('login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,password})});$('login-password').value='';await enter();}catch(err){$('login-error').textContent=err.message;}finally{button.disabled=false;}});
$('logout').addEventListener('click',async()=>{try{await api('logout',{method:'POST'});showLogin();}catch(err){$('app-error').textContent=err.message;}});
function rank(id,items,empty='（空用户名）'){const list=$(id);list.replaceChildren();if(!items.length){list.append(el('li','暂无数据','empty'));return;}items.forEach((item,i)=>{const row=el('li');const name=el('span',item.name||empty,'rank-name');name.title=item.name;row.append(el('span',String(i+1).padStart(2,'0'),'rank-number'),name,el('span',number(item.count),'rank-count'));list.append(row);});}
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
let chartFrame=0;
addEventListener('resize',()=>{cancelAnimationFrame(chartFrame);chartFrame=requestAnimationFrame(()=>{if(state.daily&&state.view==='overview')chart(state.daily);});});
async function overview(){const data=await api('stats');const s=data.stored;const live=data.runtime.live;const cards=[['累计连接',s.connections],['当前在线',data.runtime.active],['拖延中 IP',live.delaying_ips],['拖延中连接',live.delaying_connections],['认证尝试',s.attempts],['独立来源 IP',s.unique_ips],['今日连接 · UTC',s.today],['接入拒绝',s.rejected]];$('cards').replaceChildren(...cards.map(([label,value])=>{const card=el('div',undefined,'card');card.append(el('div',label,'label'),el('div',number(value),'value'));return card;}));rank('rank-sources',s.sources);rank('rank-users',s.usernames);rank('rank-methods',s.methods);rank('rank-passwords',s.passwords,'（空密码）');renderIPDelays(data.ip_delays);chart(s.daily);renderLive(live);$('storage-health').textContent='实时指标与榜单按当前在线连接计算 · 历史统计范围：当前保留的数据 · 本次进程存储错误：'+number(data.runtime.storage_errors);}
async function connections(){
  const form=new FormData($('filter-form'));const query=new URLSearchParams({page:String(state.page),limit:String(state.limit)});for(const[k,v]of form)if(v)query.set(k,v);
  const data=await api('ips?'+query);state.total=data.total;state.page=data.page;const pages=Math.max(1,Math.ceil(data.total/data.limit));
  const body=$('connection-rows');body.replaceChildren();
  if(!data.items.length){const row=el('tr');const cell=el('td','暂无连接记录','empty');cell.colSpan=7;row.append(cell);body.append(row);}
  const cell=(label,text)=>{const n=el('td',text);n.dataset.label=label;return n;};
  for(const c of data.items){const row=el('tr');const source=cell('来源 IP',c.ip);source.className='ip-source';if(c.client_mark==='parallel_slow')source.append(el('span','同 IP 并发慢速策略','badge client-mark'));row.append(source,cell('连接次数',number(c.connections)),cell('当前在线',number(c.active)),cell('最近连接',date(c.last_seen)),cell('认证次数',number(c.auth_attempts)),cell('最长在线时长',liveDuration(c.longest_ms)));const action=cell('操作');const button=el('button','查看连接','quiet');button.addEventListener('click',()=>ipDetail(c.ip));action.append(button);row.append(action);body.append(row);}
  $('record-count').textContent='共 '+number(data.total)+' 个 IP';$('page-number').textContent=state.page+' / '+pages;for(const id of ['previous','latest'])$(id).disabled=state.page<=1;for(const id of ['next','oldest'])$(id).disabled=state.page>=pages;
}
const ipState={ip:'',page:1,total:0,limit:25,filter:{},request:0};
async function ipDetail(ip){ipState.ip=ip;ipState.page=1;ipState.filter=Object.fromEntries(new FormData($('filter-form')));$('ip-title').textContent=ip;$('ip-rows').replaceChildren();if(!$('ip-dialog').open)$('ip-dialog').showModal();await ipConnections();}
async function ipConnections(){
  const request=++ipState.request;const query=new URLSearchParams({...ipState.filter,ip:ipState.ip,page:String(ipState.page),limit:String(ipState.limit)});
  try{const data=await api('connections?'+query);if(request!==ipState.request)return;$('ip-error').textContent='';ipState.page=data.page;ipState.total=data.total;const body=$('ip-rows');body.replaceChildren();
  const cell=(label,text)=>{const n=el('td',text);n.dataset.label=label;return n;};
  if(!data.items.length){const row=el('tr');const value=cell('','暂无连接记录');value.colSpan=7;row.append(value);body.append(row);}
  for(const c of data.items){const row=el('tr');row.append(cell('连接编号','#'+c.id),cell('来源',c.peer),cell('连接时间',date(c.started_at)));const status=cell('状态');status.append(el('span',statusNames[c.status]||c.status,'badge'+(!c.ended_at?' active':'')));if(c.client_mark)status.append(el('span',clientMark(c),'badge client-mark'));row.append(status,cell('认证次数',number(c.auth_attempts)),cell('持续时间',liveDuration(c.duration_ms)));const action=cell('操作');const button=el('button','认证详情','quiet');button.addEventListener('click',()=>detail(c.id));action.append(button);row.append(action);body.append(row);}
  const pages=Math.max(1,Math.ceil(data.total/data.limit));$('ip-count').textContent='共 '+number(data.total)+' 条连接';$('ip-page-number').textContent=data.page+' / '+pages;for(const id of ['ip-latest','ip-previous'])$(id).disabled=data.page<=1;for(const id of ['ip-oldest','ip-next'])$(id).disabled=data.page>=pages;
  }catch(err){$('ip-error').textContent=err.message;}
}
$('close-ip').addEventListener('click',()=>$('ip-dialog').close());
for(const [id,destination]of [['ip-latest',()=>1],['ip-previous',()=>Math.max(1,ipState.page-1)],['ip-next',()=>ipState.page+1],['ip-oldest',()=> 'last']])$(id).addEventListener('click',()=>{ipState.page=destination();ipConnections();});
async function detail(id){try{const {connection:c,attempts}=await api('connections/'+id);$('detail-title').textContent='#'+c.id+' · '+c.ip;const meta=$('detail-meta');meta.replaceChildren();for(const [label,value] of [['来源地址',c.peer],['连接时间',date(c.started_at)],['结束时间',date(c.ended_at)],['状态',statusNames[c.status]||c.status],['关闭原因',c.reason==='connection_capacity'?'连接数达上限，按需回收':c.reason==='peer_closed_during_cooldown_auth_delay'?'冷却重连认证等待期间由客户端中断':c.reason==='peer_closed_during_auth_delay'?'认证等待期间由客户端中断':c.reason||'—'],['协议模式',c.mode],['尝试用户名',c.username||'—'],['客户端标识',c.client_version||'—'],['客户端标记',clientMark(c)],['标记到期',date(c.mark_expires_at)],['发送 / 接收',number(c.bytes_sent)+' / '+number(c.bytes_received)+' bytes']])meta.append(el('dt',label),el('dd',value));const list=$('detail-attempts');list.replaceChildren();if(!attempts.length)list.append(el('p','此连接未提交认证信息。','empty'));for(const a of attempts){const item=el('article',undefined,'attempt');const head=el('header');head.append(el('span',a.method+' · '+(a.result==='simulated_success'?'仿真成功':a.result)),el('span',date(a.at)));item.append(head,el('p','用户名：'+(a.username||'（空）')));if(a.fingerprint)item.append(el('p','公钥指纹：'+a.fingerprint));item.append(el('p',a.public_key?'公钥正文：':(a.method==='password'?'密码：':'交互回答：')+(a.encoding==='base64'?'（Base64 原始字节）':''),'credential-label'),el('pre',a.public_key||a.credential||(a.method==='password'?'（空密码）':'（无内容）'),'credential-value'));if(a.truncated)item.append(el('p','记录超出字段上限，内容已截断。','muted'));list.append(item);}renderSessionTrace(c);if(!$('detail-dialog').open)$('detail-dialog').showModal();}catch(err){$('app-error').textContent=err.message;}}
$('close-detail').addEventListener('click',()=>$('detail-dialog').close());
async function refresh(){if(!state.loggedIn)return;if(state.busy){state.pending=true;return;}state.busy=true;try{$('app-error').textContent='';if(state.view==='overview')await overview();else if(state.view==='connections')await connections();else await loadSettings();$('updated').textContent='更新于 '+new Date().toLocaleTimeString('zh-CN',{hour12:false});}catch(err){$('app-error').textContent=err.message;}finally{state.busy=false;if(state.pending){state.pending=false;refresh();}}}
function tab(view){state.view=view;$('overview').hidden=view!=='overview';$('connections-view').hidden=view!=='connections';$('settings-view').hidden=view!=='settings';for(const v of ['overview','connections','settings']){$('tab-'+v).classList.toggle('active',v===view);$('tab-'+v).setAttribute('aria-selected',String(v===view));}refresh();}
$('tab-overview').addEventListener('click',()=>tab('overview'));$('tab-connections').addEventListener('click',()=>tab('connections'));$('refresh').addEventListener('click',refresh);
$('filter-form').addEventListener('submit',e=>{e.preventDefault();state.page=1;refresh();});$('filter-form').addEventListener('reset',()=>{state.page=1;setTimeout(refresh,0);});$('previous').addEventListener('click',()=>{state.page--;refresh();});$('next').addEventListener('click',()=>{state.page++;refresh();});
$('latest').addEventListener('click',()=>{state.page=1;refresh();});$('oldest').addEventListener('click',()=>{state.page='last';refresh();});
setInterval(()=>{if(state.autoUpdate&&state.view!=='settings'&&!document.hidden&&!$('detail-dialog').open&&!$('ip-dialog').open)refresh();},10000);
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
  $('strategy-description').textContent=banner?'首行等待 '+durationLabel(policy.delay_ms)+' 为基准，每次加入 ±'+number(policy.delay_jitter_percent)+'% 随机扰动后发送横幅。':'先完成 SSH 握手，接收认证后等待 '+durationLabel(policy.delay_ms)+' 为基准，加入 ±'+number(policy.delay_jitter_percent)+'% 随机扰动后拒绝认证。';
  const retryItems=[['单次认证重试阈值',policy.single_auth_retry_threshold===0?'禁用':number(policy.single_auth_retry_threshold)+' 次连续单次密码拒绝后退出'],['单次认证重试窗口',durationLabel(policy.single_auth_retry_window_ms)+' · 按 IP 与完整 SSH 标识'],['单次认证重试冷却',durationLabel(policy.single_auth_retry_cooldown_ms)+' · 重连不续期']];
  const items=[...retryItems,[banner?'首行 / 行间隔':'每次认证延迟',durationLabel(policy.delay_ms)],['普通拖延随机范围',durationLabel(policy.delay_min_ms)+' – '+durationLabel(policy.delay_max_ms)+'（±'+number(policy.delay_jitter_percent)+'%）'],banner?['横幅最大行长',number(policy.max_line_length)+' 字节']:['单连接认证上限',policy.max_auth_tries?number(policy.max_auth_tries)+' 次':'不限次数'],['连接最长持续',policy.slow_connection_lifetime==='unlimited'?'不限时':durationLabel(policy.max_connection_time_ms)],['满载回收','新连接到达且容量满时，回收保持最久的慢速连接'],['握手超时',durationLabel(policy.handshake_timeout_ms)],['认证空闲超时',durationLabel(policy.idle_timeout_ms)],['认证中断标记冷却',durationLabel(policy.client_cooldown_ms)],['冷却重连认证初始等待',durationLabel(policy.cooldown_auth_delay_ms)],['自适应认证范围',durationLabel(policy.cooldown_auth_delay_min_ms)+' – '+durationLabel(policy.cooldown_auth_delay_max_ms)],['认证等待调整','提前退出减少、继续会话增加 '+durationLabel(policy.cooldown_auth_delay_step_ms)+'；每连接一次'],['认证等待随机扰动','±'+number(policy.cooldown_auth_jitter_percent)+'%，实际值不低于下限'],['首次中断采样','首次及冷却重连的本次认证等待减去一步调整量，直接下调；仍守下限'],['增加认证等待的条件','shell / exec 接受后保持至少 '+durationLabel(policy.cooldown_auth_success_hold_ms)+'；仅开通道或保活不计'],['认证等待学习','仅存内存，标记冷却后废弃，重启重置'],['冷却期间重连',policy.cooldown_return_mode==='network_jitter'?'延迟认证后仿真登录，响应随机迟滞':'认证成功后立即断开'],['同 IP 并发判定',durationLabel(policy.parallel_window_ms)+' 内 '+number(policy.parallel_connections)+' 条重叠连接'],['并发 IP 处理','全部连接延迟拒绝，不标记认证超时'],['并发慢速标记冷却','最后连接断开后闲置 '+durationLabel(policy.parallel_cooldown_ms)],['探针识别','同 IP 连续 '+number(policy.probe_failure_threshold)+' 次握手未完成'],['探针冷却',durationLabel(policy.probe_cooldown_ms)+'，静默丢弃，不续期'],['总连接上限',number(policy.max_clients)],['每 IP 并发上限',policy.max_clients_per_ip?number(policy.max_clients_per_ip):'不限制'],['每 IP 每分钟接入',policy.connections_per_minute?number(policy.connections_per_minute):'不限制']];
  if(!banner&&policy.network_jitter){const p=policy.network_jitter;items.push(['仿真响应常规延迟',durationLabel(p.short_min_ms)+' – '+durationLabel(p.short_max_ms)],['仿真响应偶发停顿',durationLabel(p.stall_min_ms)+' – '+durationLabel(p.stall_max_ms)+'（'+number(p.stall_percent)+'%）']);}
  for(const[label,value]of items){const pair=el('div');pair.append(el('dt',label),el('dd',value));fields.append(pair);}
}
let resizeTimer=0;addEventListener('resize',()=>{clearTimeout(resizeTimer);resizeTimer=setTimeout(()=>{if(state.loggedIn&&state.view==='overview'&&state.daily){chart(state.daily);}},100);});
$('auto-refresh').addEventListener('click',()=>{state.autoUpdate=!state.autoUpdate;try{localStorage.setItem('wzxjdssh-auto-update',String(state.autoUpdate));}catch{};renderAutoUpdate();if(state.autoUpdate)refresh();});
async function loadSettings(){const [settings,stats]=await Promise.all([api('settings'),api('stats')]);$('idle-minutes').value=settings.idle_timeout_minutes;state.idleTimeout=settings.idle_timeout_seconds*1000;renderStrategy(stats.runtime.strategy);}
$('tab-settings').addEventListener('click',()=>tab('settings'));
$('settings-form').addEventListener('submit',async event=>{event.preventDefault();const button=event.currentTarget.querySelector('button');const message=$('settings-message');button.disabled=true;message.textContent='';try{const settings=await api('settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({idle_timeout_minutes:Number($('idle-minutes').value)})});state.idleTimeout=settings.idle_timeout_seconds*1000;state.lastActivity=Date.now();$('idle-minutes').value=settings.idle_timeout_minutes;message.className='muted';message.textContent='设置已保存并生效。';try{localStorage.setItem('wzxjdssh-idle-timeout:'+location.pathname,String(state.idleTimeout));}catch{}}catch(err){message.className='error';message.textContent=err.message;}finally{button.disabled=false;}});
addEventListener('storage',event=>{if(event.key==='wzxjdssh-idle-timeout:'+location.pathname){const ms=Number(event.newValue);if(ms>=60000&&ms<=86400000)state.idleTimeout=ms;}});

function liveDuration(ms){
	ms=Number.isFinite(ms)?Math.max(0,ms):0;
  const seconds=Math.floor(Math.max(0,ms)/1000);
  if(seconds<60)return (ms/1000).toFixed(1)+' 秒';
  if(seconds<3600)return Math.floor(seconds/60)+' 分 '+seconds%60+' 秒';
  return Math.floor(seconds/3600)+' 小时 '+Math.floor(seconds%3600/60)+' 分';
}
function renderLive(live){
  const delays=$('rank-delays');delays.replaceChildren();
  if(!live.longest_connections.length)delays.append(el('li','暂无在线连接','empty'));
  live.longest_connections.forEach((item,index)=>{
    const row=el('li');const info=el('div',undefined,'live-rank-info');const source=el('div',item.peer,'live-source');
    const meta=el('div','在线 '+liveDuration(item.duration_ms)+' · '+(item.delaying?'正在延迟响应':item.delayed_ms?'等待客户端':'握手 / 等待认证'),'muted');
    info.append(source,meta);
    if(item.connection_id){const button=el('button','#'+item.connection_id+' · 详情','quiet live-detail');button.addEventListener('click',()=>detail(item.connection_id));info.append(button);}
    row.append(el('span',String(index+1).padStart(2,'0'),'rank-number'),info,el('span',liveDuration(item.delayed_ms),'rank-count'));delays.append(row);
  });
  const concurrency=$('rank-concurrency');concurrency.replaceChildren();
  if(!live.ip_concurrency.length)concurrency.append(el('li','暂无在线连接','empty'));
  live.ip_concurrency.forEach((item,index)=>{
    const row=el('li');const info=el('div',undefined,'live-rank-info');info.append(el('div',item.ip,'live-source'),el('div','其中拖延中 '+number(item.delaying_connections)+' 条','muted'));
    row.append(el('span',String(index+1).padStart(2,'0'),'rank-number'),info,el('span',number(item.connections)+' 条','rank-count'));concurrency.append(row);
  });
}

function clientMark(c){if(c.client_mark==='handshake_probe')return '握手探针 · 静默冷却至 '+date(c.mark_expires_at);if(c.client_mark==='parallel_slow')return '同 IP 并发 · 慢速策略';const labels={single_auth_retry:'单次认证反复重试',auth_interrupted:'认证延迟中断（可能超时）',cooldown:'冷却标记',simulated_success:'旧版仿真后断开',simulated_network:'仿真网络迟滞'};return c.client_mark?(labels[c.client_mark]||c.client_mark)+(c.mark_expires_at<Date.now()?' · 已冷却':' · 冷却中'):'—';}

function renderIPDelays(items){const list=$('rank-ip-delays');list.replaceChildren();if(!items.length)list.append(el('li','暂无拖延记录','empty'));items.forEach((item,index)=>{const row=el('li');const info=el('div',undefined,'live-rank-info');info.append(el('div',item.ip,'live-source'));const meta=el('div',undefined,'rank-inline-meta');meta.append(el('span','累计 '+liveDuration(item.delayed_ms),'muted'));const button=el('button','查看连接','quiet live-detail');button.addEventListener('click',()=>{ipState.filter={};ipState.ip=item.ip;ipState.page=1;$('ip-title').textContent=item.ip;if(!$('ip-dialog').open)$('ip-dialog').showModal();ipConnections();});meta.append(button);info.append(meta);row.append(el('span',String(index+1).padStart(2,'0'),'rank-number'),info,el('span',liveDuration(item.longest_ms),'rank-count'));list.append(row);});}

const sessionStages={authenticated:'仿真认证完成',global_request:'收到全局请求',channel_requested:'收到通道请求',channel_rejected:'通道被拒绝',channel_confirmation_failed:'通道确认未送达',channel_confirmed:'通道确认已发送',request_received:'收到会话请求',request_reply_failed:'会话请求回复失败',request_accepted:'会话请求已接受',request_rejected:'会话请求被拒绝',channel_closed:'会话通道已关闭',session_held:'会话保持达到学习门槛'};
function renderSessionTrace(c){const section=$('detail-session');const trace=c.session;section.hidden=!trace&&!['simulated_network','simulated_closed'].includes(c.status);const meta=$('session-meta'),events=$('session-events');meta.replaceChildren();events.replaceChildren();if(!trace){if(!section.hidden){meta.append(el('dt','阶段记录'),el('dd','此历史连接未采集阶段信息。'));}return;}for(const [label,value]of [['仿真认证完成',date(trace.authenticated_at)],['认证前耗时',liveDuration(Math.max(0,trace.authenticated_at-c.started_at))],['认证后停留',liveDuration(trace.post_auth_ms)],['最后记录阶段',sessionStages[trace.last_stage]||trace.last_stage],['结束信息',trace.ended_at?(trace.end_error||'连接结束'):'连接仍在线，时长为最近阶段快照']])meta.append(el('dt',label),el('dd',value));for(const e of trace.events){const item=el('li');item.append(el('span','+'+liveDuration(Math.max(0,e.at-trace.authenticated_at)),'muted'),el('span',(sessionStages[e.stage]||e.stage)+(e.kind?' · '+e.kind:'')));events.append(item);}}
