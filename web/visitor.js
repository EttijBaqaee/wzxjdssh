(() => {
 'use strict';
 const box=el('div',undefined,'visitor-info'),identity=el('span'),check=el('span',undefined,'visitor-check'),comparison=el('span',undefined,'visitor-comparison');
 box.id='visitor-info';box.setAttribute('role','status');box.append(identity,check,comparison);
 document.querySelector('.header-meta').prepend(box);
 let generation=0,timer=0,controller=null;
 function render(data,pending=false,failed=false){
  const geo=data.geo,asn=geo?.asn?'AS'+geo.asn+(geo.organization?' · '+geo.organization:''):t('ASN 暂不可用');
  identity.textContent=t('你的 IP：')+(data.ip||'—')+' · '+geoSummary(geo)+' · '+asn;
  const mismatch=data.verification==='mismatch';
  check.className='visitor-check'+(mismatch?' visitor-warning':'');
  check.textContent=pending?t('正在双路校验…'):data.verification==='match'?t('双路一致'):mismatch?t('两路 IP 不一致'):failed?t('单路识别 · 访问者接口不可用'):t('单路识别');
  comparison.hidden=!mismatch;
  const source={socket:t('直接连接'),forwarded:t('可信反代'),real_ip:t('可信反代'),unavailable:t('不可用')}[data.server_source]||t('不可用');
  comparison.textContent=mismatch?t('访问者接口：')+data.endpoint_ip+' · '+source+'：'+(data.server_ip||'—'):'';
  box.title=t('第二路来源：')+source+' · '+(data.server_ip||'—')+'\n'+t('访问者接口：')+(data.endpoint_url||'/get-my-ip')+'\n'+t('校验仅比较两路观测地址，不作为身份认证。');
 }
 async function endpointIP(value,signal){
  const target=new URL(value,location.origin);
  const response=await fetch(target,{signal,credentials:target.origin===location.origin?'same-origin':'omit',cache:'no-store',referrerPolicy:'no-referrer',redirect:'error'});
  if(!response.ok||!response.body)throw new Error('visitor endpoint unavailable');
  const reader=response.body.getReader(),chunks=[];let size=0;
  try{for(;;){const {done,value:chunk}=await reader.read();if(done)break;size+=chunk.byteLength;if(size>4096)throw new Error('visitor response too large');chunks.push(chunk);}}
  finally{await reader.cancel().catch(()=>{});reader.releaseLock();}
  const bytes=new Uint8Array(size);let offset=0;for(const chunk of chunks){bytes.set(chunk,offset);offset+=chunk.byteLength;}
  const text=new TextDecoder().decode(bytes).trim();let data;
  try{data=JSON.parse(text);}catch{data=text;}
  if(data&&typeof data==='object')data=data.ip??data.client_ip??data.clientIp??data.address??data.query??data.remote_addr??data.data;
  if(data&&typeof data==='object')data=data.ip??data.address;
  if(typeof data!=='string'||!data.trim()||data.length>128)throw new Error('invalid visitor address');
  return data.trim();
 }
 async function refreshVisitor(){
  if(!state.loggedIn||document.hidden||controller)return;
  const current=generation,abort=new AbortController();controller=abort;let expiry=0,baseline;
  try{
   expiry=setTimeout(()=>abort.abort(),5000);
   baseline=await api('visitor',{signal:abort.signal});
   if(current!==generation||!state.loggedIn)return;
   render(baseline,true);
   const ip=await endpointIP(baseline.endpoint_url||'/get-my-ip',abort.signal);
   const verified=await api('visitor?ip='+encodeURIComponent(ip),{signal:abort.signal});
   if(current===generation&&state.loggedIn)render(verified);
  }catch{
   if(current===generation&&state.loggedIn){if(baseline)render(baseline,false,true);else{identity.textContent=t('来访者 IP 暂不可用');check.textContent='';comparison.hidden=true;}}
  }finally{
   clearTimeout(expiry);if(controller===abort)controller=null;
   if(current===generation&&state.loggedIn){clearTimeout(timer);timer=setTimeout(refreshVisitor,300000);}
  }
 }
 document.addEventListener('console-login',refreshVisitor);
 document.addEventListener('console-logout',()=>{generation++;clearTimeout(timer);controller?.abort();controller=null;box.removeAttribute('title');identity.textContent='';check.textContent='';comparison.textContent='';});
 document.addEventListener('visibilitychange',()=>{if(!document.hidden&&state.loggedIn){clearTimeout(timer);refreshVisitor();}});
 if(state.loggedIn)refreshVisitor();
})();
