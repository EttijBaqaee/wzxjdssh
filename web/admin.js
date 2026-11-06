'use strict';
let clearingDatabase=false,updatingIPDB=false,clearPoll=0;
function renderIPDB(data){
 updatingIPDB=!!data.updating;
 $('ipdb-update').disabled=!data.enabled||updatingIPDB;
 $('ipdb-update').textContent=updatingIPDB?t('正在更新…'):t('更新数据库');
 const list=$('ipdb-status');list.replaceChildren();
 if(!data.enabled){list.append(el('p',t('IP 归属查询未启用。'),'muted'));return;}
 for(const item of data.databases||[]){const row=el('div',undefined,'ipdb-row');row.append(el('strong',item.name),el('span',item.ready?t('已加载 · ')+number(item.bytes/1048576)+' MiB':t('未加载'),item.ready?'':'error'),el('span',item.ready?t('库构建于 ')+date(item.build_at):I18n.serverText(item.error)||t('等待后台下载'),'muted'));if(item.ready&&item.error)row.append(el('span',I18n.serverText(item.error),'error'));list.append(row);}
 list.append(el('p',t('缓存 {entries} / {max} 项 · 闲置 {seconds} 秒失效',{entries:number(data.cache_entries),max:number(data.cache_max),seconds:number(data.cache_ttl_seconds)})+' · '+(data.auto_update?t('每 {days} 天更新过期库',{days:number(data.update_days)}):t('自动更新已关闭')),'muted'));
}
function renderClearDB(data){
 const wasRunning=clearingDatabase;clearingDatabase=!!data.running;$('clear-database').disabled=clearingDatabase;
 $('clear-db-status').textContent=clearingDatabase?t('正在释放连接并清理记录，请稍候…'):I18n.serverText(data.error)||(data.cleared?t('已清空 {count} 条连接及关联认证、标签记录。',{count:number(data.deleted_connections)}):'');
 $('clear-db-status').className=data.error?'error':'muted';
 if(wasRunning&&!clearingDatabase){state.page=1;for(const id of ['detail-dialog','ip-dialog'])if($(id).open)$(id).close();$('detail-attempts').replaceChildren();api('emergency').then(renderEmergency).catch(()=>{});}
 if(clearingDatabase&&!clearPoll)clearPoll=setTimeout(async()=>{clearPoll=0;try{renderClearDB(await api('database/clear'));}catch(err){$('clear-db-status').textContent=err.message;}},1000);
}
let bannerDirty=false,bannerLoaded=false,pendingBanner='';
function renderBanner(data,replaceInput=false){
 $('banner-current').textContent=data.server_version;
 if(replaceInput||!bannerDirty){$('banner-value').value=data.server_version.slice('SSH-2.0-'.length);bannerDirty=false;}
 bannerLoaded=true;$('banner-save').disabled=false;
}
async function loadDatabaseTools(){const [ipdb,clear,banner]=await Promise.all([api('ipdb'),api('database/clear'),api('banner')]);renderIPDB(ipdb);renderClearDB(clear);renderBanner(banner);}
$('banner-value').addEventListener('input',()=>{bannerDirty=true;$('banner-message').textContent='';});
$('banner-form').addEventListener('submit',async event=>{
 event.preventDefault();if(!bannerLoaded)return;pendingBanner='SSH-2.0-'+$('banner-value').value;$('banner-pending').textContent=pendingBanner;$('banner-error').textContent='';$('banner-password').value='';$('banner-dialog').showModal();$('banner-password').focus();
});
$('cancel-banner').addEventListener('click',()=>$('banner-dialog').close());
$('banner-dialog').addEventListener('close',()=>{$('banner-password').value='';$('banner-error').textContent='';$('banner-pending').textContent='';pendingBanner='';});
$('banner-confirm-form').addEventListener('submit',async event=>{
 event.preventDefault();const button=$('banner-confirm'),message=$('banner-message');button.disabled=true;$('banner-error').textContent='';message.textContent='';
 const password=$('banner-password').value.trim(),server_version=pendingBanner;$('banner-password').value='';
 try{const data=await api('banner',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password,server_version})});$('banner-dialog').close();renderBanner(data,true);message.className='muted';message.textContent=t('横幅已修改，新 SSH 连接立即生效。');}
 catch(err){$('banner-error').textContent=err.message;}
 finally{button.disabled=false;}
});
document.addEventListener('console-logout',()=>{if($('banner-dialog').open)$('banner-dialog').close();$('banner-password').value='';$('banner-value').value='';$('banner-current').textContent='—';$('banner-message').textContent='';bannerDirty=false;bannerLoaded=false;$('banner-save').disabled=true;});
$('ipdb-update').addEventListener('click',async()=>{const button=$('ipdb-update');button.disabled=true;$('ipdb-message').textContent='';try{renderIPDB(await api('ipdb/update',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}));$('ipdb-message').textContent=t('后台更新已提交；下载失败时保留原库，SSH 服务不受影响。');}catch(err){$('ipdb-message').textContent=err.message;button.disabled=false;}});
$('clear-database').addEventListener('click',()=>{$('clear-db-error').textContent='';$('clear-db-password').value='';$('clear-db-dialog').showModal();$('clear-db-password').focus();});
$('cancel-clear-db').addEventListener('click',()=>$('clear-db-dialog').close());
$('clear-db-dialog').addEventListener('close',()=>{$('clear-db-password').value='';$('clear-db-error').textContent='';});
$('clear-db-form').addEventListener('submit',async event=>{
 event.preventDefault();const button=event.currentTarget.querySelector('button');button.disabled=true;$('clear-db-error').textContent='';
 const password=$('clear-db-password').value.trim();$('clear-db-password').value='';
 try{const data=await api('database/clear',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password,confirm:'CLEAR_DATABASE'})});$('clear-db-dialog').close();renderClearDB(data);}catch(err){$('clear-db-error').textContent=err.message;}finally{button.disabled=false;}
});
setInterval(()=>{if(state.loggedIn&&state.view==='settings'&&!document.hidden&&updatingIPDB)api('ipdb').then(renderIPDB).catch(()=>{});},3000);
