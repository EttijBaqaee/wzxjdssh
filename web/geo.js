'use strict';
function geoSummary(geo){
 if(!geo)return t('归属数据尚未就绪');
 if(geo.status!=='found')return {disabled:t('归属查询未启用'),private:t('内网 / 本地地址'),invalid:t('无效 IP'),unavailable:t('归属数据库尚未就绪'),not_found:t('数据库未收录')}[geo.status]||t('暂无归属信息');
 const places=[geo.country,geo.province,geo.city,geo.district].filter((v,i,a)=>v&&a.indexOf(v)===i);
 return [...places,geo.isp,geo.network_type].filter(Boolean).join(' · ')||(geo.asn?'AS'+geo.asn:t('暂无地区信息'));
}
function geoNode(ip,geo){const node=document.createElement('span');node.className='ip-geo';node.textContent=geoSummary(geo||(typeof state!=='undefined'?state.geo?.[ip]:null));return node;}
