'use strict';
let releaseTimer=0,releaseBusy=false,releaseGeneration=0;
const releasePanel=el('section',undefined,'panel release-panel');
const releaseHeading=el('div',undefined,'panel-heading');
const releaseButton=el('button',t('检查更新'),'quiet');releaseButton.type='button';releaseButton.id='release-check';
releaseHeading.append(el('h2',t('版本更新')),releaseButton);
const releaseMessage=el('p',t('登录后自动检查正式版本。'),'muted');releaseMessage.id='release-status';releaseMessage.setAttribute('role','status');
releasePanel.append(releaseHeading,releaseMessage);$('settings-view').prepend(releasePanel);
function renderRelease(data){
 $('version-new').hidden=!data.update_available;
 $('version-new').title=data.latest?t('发现新版本 ')+data.latest+t('，前往 GitHub 查看'):'';
 releaseButton.disabled=!!data.checking;
 releaseButton.textContent=data.checking?t('正在检查…'):t('检查更新');
 const known=data.latest?t('最新正式版本 ')+data.latest+t(' · 当前 ')+data.current:t('当前 ')+data.current;
 releaseMessage.textContent=data.checking?known+t(' · 正在检查…'):data.error?known+' · '+I18n.serverText(data.error):known+(data.update_available?t(' · 有新版本可用'):t(' · 已是最新版本'))+(data.checked_at?t(' · 检查于 ')+date(data.checked_at):'');
 releaseMessage.className=data.error?'error':'muted';
}
async function checkRelease(force=false){
 if(!state.loggedIn||releaseBusy)return;
 clearTimeout(releaseTimer);releaseBusy=true;const generation=releaseGeneration;let delay=60000;
 try{const data=await api(force?'version/check':'version',force?{method:'POST'}:{});if(generation!==releaseGeneration||!state.loggedIn)return;renderRelease(data);if(data.checking)delay=1500;}
 catch(err){if(generation===releaseGeneration&&state.loggedIn){releaseMessage.textContent=err.message;releaseMessage.className='error';releaseButton.disabled=false;releaseButton.textContent=t('检查更新');}}
 finally{releaseBusy=false;if(state.loggedIn)releaseTimer=setTimeout(()=>checkRelease(),generation===releaseGeneration?delay:0);}
}
releaseButton.addEventListener('click',()=>checkRelease(true));
document.addEventListener('console-login',()=>checkRelease());
document.addEventListener('console-logout',()=>{releaseGeneration++;clearTimeout(releaseTimer);$('version-new').hidden=true;});
if(state.loggedIn)checkRelease();
