'use strict';
let clearingDatabase=false,updatingIPDB=false,clearPoll=0;
function renderIPDB(data){
 updatingIPDB=!!data.updating;
 $('ipdb-update').disabled=!data.enabled||updatingIPDB;
 $('ipdb-update').textContent=updatingIPDB?'正在更新…':'更新数据库';
 const list=$('ipdb-status');list.replaceChildren();
 if(!data.enabled){list.append(el('p','IP 归属查询未启用。','muted'));return;}
 for(const item of data.databases||[]){const row=el('div',undefined,'ipdb-row');row.append(el('strong',item.name),el('span',item.ready?'已加载 · '+number(item.bytes/1048576)+' MiB':'未加载',item.ready?'':'error'),el('span',item.ready?'库构建于 '+date(item.build_at):item.error||'等待后台下载','muted'));if(item.ready&&item.error)row.append(el('span',item.error,'error'));list.append(row);}
 list.append(el('p','缓存 '+number(data.cache_entries)+' / '+number(data.cache_max)+' 项 · 闲置 '+number(data.cache_ttl_seconds)+' 秒失效 · '+(data.auto_update?'每 '+number(data.update_days)+' 天更新过期库':'自动更新已关闭'),'muted'));
}
function renderClearDB(data){
 const wasRunning=clearingDatabase;clearingDatabase=!!data.running;$('clear-database').disabled=clearingDatabase;
 $('clear-db-status').textContent=clearingDatabase?'正在释放连接并清理记录，请稍候…':data.error||(data.cleared?'已清空 '+number(data.deleted_connections)+' 条连接及关联认证、标签记录。':'');
 $('clear-db-status').className=data.error?'error':'muted';
 if(wasRunning&&!clearingDatabase){state.page=1;for(const id of ['detail-dialog','ip-dialog'])if($(id).open)$(id).close();$('detail-attempts').replaceChildren();api('emergency').then(renderEmergency).catch(()=>{});}
 if(clearingDatabase&&!clearPoll)clearPoll=setTimeout(async()=>{clearPoll=0;try{renderClearDB(await api('database/clear'));}catch(err){$('clear-db-status').textContent=err.message;}},1000);
}
async function loadDatabaseTools(){const [ipdb,clear]=await Promise.all([api('ipdb'),api('database/clear')]);renderIPDB(ipdb);renderClearDB(clear);}
$('ipdb-update').addEventListener('click',async()=>{const button=$('ipdb-update');button.disabled=true;$('ipdb-message').textContent='';try{renderIPDB(await api('ipdb/update',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}));$('ipdb-message').textContent='后台更新已提交；下载失败时保留原库，SSH 服务不受影响。';}catch(err){$('ipdb-message').textContent=err.message;button.disabled=false;}});
$('clear-database').addEventListener('click',()=>{$('clear-db-error').textContent='';$('clear-db-password').value='';$('clear-db-dialog').showModal();$('clear-db-password').focus();});
$('cancel-clear-db').addEventListener('click',()=>$('clear-db-dialog').close());
$('clear-db-dialog').addEventListener('close',()=>{$('clear-db-password').value='';$('clear-db-error').textContent='';});
$('clear-db-form').addEventListener('submit',async event=>{
 event.preventDefault();const button=event.currentTarget.querySelector('button');button.disabled=true;$('clear-db-error').textContent='';
 const password=$('clear-db-password').value.trim();$('clear-db-password').value='';
 try{const data=await api('database/clear',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password,confirm:'CLEAR_DATABASE'})});$('clear-db-dialog').close();renderClearDB(data);}catch(err){$('clear-db-error').textContent=err.message;}finally{button.disabled=false;}
});
setInterval(()=>{if(state.loggedIn&&state.view==='settings'&&!document.hidden&&updatingIPDB)api('ipdb').then(renderIPDB).catch(()=>{});},3000);
