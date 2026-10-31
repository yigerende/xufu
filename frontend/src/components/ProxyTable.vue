<script setup lang="ts">
import {ref,onUnmounted} from 'vue'
import {ShieldCheck,RefreshCw,Globe2} from 'lucide-vue-next'
import {api,date,labels,notify,regionLabel,type Row} from '../lib'
import DataTable from './DataTable.vue'
import Modal from './Modal.vue'
const props=defineProps<{rows:Row[];busy:boolean}>()
const emit=defineEmits<{refresh:[];edit:[Row];remove:[Row]}>()
const modal=ref(false),title=ref(''),check=ref<Row>({}),error=ref(''),working=ref(false)
const statusNames:Row={pass:'通过',warn:'需关注',challenge:'遇到验证',fail:'失败'}
const stabilityNames:Row={stable:'两次出口一致',changed:'两次出口不同',unknown:'暂无法判断'}
const stageNames:Row={proxy_connect:'代理连接 / 隧道建立',tls:'TLS 握手',response:'等待目标响应',body:'读取目标响应'}
let poll:ReturnType<typeof setTimeout>|undefined,revision=0
const controller=new AbortController()
onUnmounted(()=>{revision++;clearTimeout(poll);controller.abort()})
const signal=()=>AbortSignal.any([controller.signal,AbortSignal.timeout(12000)])
async function setGlobal(row:Row){
 if(working.value||props.busy)return;working.value=true
 try{await api('/admin/resources/'+row.id+'/global','POST',{}, {},signal());notify('已设为全局代理，续费管理菜单使用此代理');emit('refresh')}catch(e){notify((e as Error).message)}finally{working.value=false}
}
async function testExit(row:Row){
 if(working.value||props.busy)return;working.value=true
 try{const v=await api('/admin/resources/'+row.id+'/test','POST',{}, {},AbortSignal.any([controller.signal,AbortSignal.timeout(20000)]));notify(v.ok?`出口 ${v.country||'—'} · ${v.ip} · ${v.latency_ms} ms`:v.message);emit('refresh')}catch(e){notify((e as Error).message)}finally{working.value=false}
}
async function readCheck(id:string,rev:number){
 try{const v=await api('/admin/proxy-checks/'+id,'GET',undefined,{},signal());if(rev!==revision)return;check.value=v;error.value='';if(v.state!=='completed')poll=setTimeout(()=>readCheck(id,rev),2000);else emit('refresh')}
 catch(e){if(rev!==revision)return;error.value=(e as Error).message;poll=setTimeout(()=>readCheck(id,rev),5000)}
}
async function test(row:Row,kind:string){
 if(working.value||props.busy)return
 working.value=true;const rev=++revision;clearTimeout(poll);title.value=row.name+' · '+(kind==='quality'?'质量评分':'OpenAI 测试');check.value={state:'queued'};error.value='';modal.value=true
 try{const v=await api('/admin/resources/'+row.id+'/checks','POST',{kind},{},signal());emit('refresh');if(rev===revision)await readCheck(v.id,rev)}catch(e){if(rev===revision){error.value=(e as Error).message;check.value={}}}finally{working.value=false}
}
async function viewPending(row:Row){const rev=++revision;clearTimeout(poll);title.value=row.name+' · 测试进度';modal.value=true;check.value={state:'queued'};error.value='';await readCheck(row.active_check,rev)}
function view(row:Row,kind:string){revision++;clearTimeout(poll);title.value=row.name+' · '+(kind==='quality'?'质量评分':'OpenAI 测试');check.value={state:'completed',result:kind==='quality'?row.quality_result:row.openai_result};error.value='';modal.value=true}
function close(){modal.value=false;revision++;clearTimeout(poll)}
function tone(s:string){return s==='pass'?'active':s==='fail'?'failed':'review'}
</script>
<template>
<div class="panel proxy-table"><DataTable :rows="rows" :columns="[{key:'name',label:'代理名称 / 用途'},{key:'country',label:'订阅地区'},{key:'capacity',label:'占用 / 容量'},{key:'last_ip',label:'出口 IP / 地区'},{key:'quality',label:'质量评分'},{key:'openai',label:'OpenAI'},{key:'action',label:'操作'}]">
 <template #name="{row}"><b>{{row.name}}</b><small>{{({iproyal:'IPRoyal',cliproxy:'Cliproxy',other:'其他'} as Row)[row.vendor]||'其他'}} · {{row.ip_mode==='static'?'静态':'动态'}}</small><div class="proxy-flags"><span v-if="row.is_global" class="badge active"><Globe2 :size="12"/>全局代理</span><span v-if="!row.enabled" class="badge disabled">已停用</span><span v-else-if="!row.is_global" class="muted small">已启用</span></div></template>
 <template #country="{row}">{{regionLabel(row.country)}}</template>
 <template #capacity="{row}">{{row.in_use}} / {{row.capacity}}</template>
 <template #last_ip="{row}">{{row.last_ip||'—'}}<small>{{row.last_country||'未检测'}} · {{row.last_latency_ms||0}} ms</small><small>{{labels[row.last_status]||row.last_status}} · {{date(row.last_checked)}}</small></template>
 <template #quality="{row}"><button v-if="row.quality_result?.checked_at" class="text-btn proxy-score" @click="view(row,'quality')"><b>{{row.quality_result.score??'—'}}</b><span>{{row.quality_result.grade||''}}</span></button><span v-else class="muted">未测试</span><small v-if="row.quality_result?.checked_at">{{date(row.quality_result.checked_at)}}</small></template>
 <template #openai="{row}"><button v-if="row.openai_result?.checked_at" class="badge" :class="tone(row.openai_result.status)" @click="view(row,'openai')">{{statusNames[row.openai_result.status]}}</button><span v-else class="muted">未测试</span><small v-if="row.openai_result?.checked_at">{{date(row.openai_result.checked_at)}}</small></template>
 <template #action="{row}"><div class="proxy-actions"><button v-if="!row.is_global" class="text-btn" :disabled="busy||working||!row.enabled" @click="setGlobal(row)">设为全局</button><button v-if="row.active_check" class="text-btn" @click="viewPending(row)">查看测试进度</button><template v-else><button class="text-btn" :disabled="busy||working" @click="testExit(row)">连通测试</button><button class="text-btn" :disabled="busy||working" @click="test(row,'quality')">测试评分</button><button class="text-btn" :disabled="busy||working" @click="test(row,'openai')">OpenAI 测试</button></template><button class="text-btn" :disabled="busy||working" @click="emit('edit',row)">编辑</button><button class="text-btn danger-text" :disabled="busy||working" @click="emit('remove',row)">删除</button></div></template>
</DataTable></div>
<Modal v-if="modal" :title="title" @close="close">
 <p v-if="error" class="alert danger" role="alert">{{error}}</p>
 <div v-if="['queued','running'].includes(check.state)" class="proxy-testing" role="status"><RefreshCw :size="24" class="spinning"/><h3>{{check.state==='queued'?'等待空闲测试资源':'正在后台检测'}}</h3><p>可关闭窗口，结果会保留；测试不会登录账号或取消订阅。</p></div>
 <template v-if="check.result?.checked_at">
  <div class="proxy-result-heading"><ShieldCheck :size="28"/><div><strong v-if="check.result.kind==='quality'">{{check.result.score??'—'}}<small> / 100 · {{check.result.grade||'—'}}</small></strong><h3 v-else>OpenAI {{statusNames[check.result.status]}}</h3><p>{{check.result.message}}</p><small>{{date(check.result.checked_at)}}</small></div></div>
  <template v-if="check.result.kind==='quality'"><p v-if="check.result.version!==2" class="alert">这是旧版评分，请重新测试；新版不因动态代理更换出口 IP 扣分。</p><div class="proxy-score-parts"><div v-for="part in [{key:'connectivity',name:'出口连通',max:30},{key:'stability',name:check.result.version===2?'连续连接成功':'出口稳定（旧版）',max:20},{key:'latency',name:'响应速度',max:20},{key:'openai',name:'OpenAI 可达',max:30}]" :key="part.key"><span>{{part.name}}</span><b>{{check.result.parts?.[part.key]??0}} / {{part.max}}</b></div></div><p>{{stabilityNames[check.result.stability]||'未完成出口检查'}}<span v-if="check.result.stability==='changed'&&check.result.version===2"> · 动态出口变化不扣分</span></p><div v-for="(sample,i) in check.result.samples" :key="i" class="proxy-sample"><span>出口 {{Number(i)+1}}</span><b>{{sample.ok?sample.ip:'探测失败'}}</b><small>{{sample.country||'—'}} · {{sample.latency_ms}} ms</small></div></template>
  <div class="callout"><b>OpenAI：{{statusNames[check.result.openai?.status]||'未检测'}}</b><p>{{check.result.openai?.message}}</p><small v-if="check.result.openai?.http_status">HTTP {{check.result.openai.http_status}} · {{check.result.openai.latency_ms}} ms</small><small v-else-if="check.result.openai">未收到目标 HTTP 响应 · {{check.result.openai.latency_ms}} ms</small><small v-if="check.result.openai?.stage"> · {{stageNames[check.result.openai.stage]||'网络连接'}}</small></div>
  <p class="muted small">评分仅反映本次连接质量，不是 IP 风险分。动态代理的出口探测与目标请求可能使用不同 IP；展示的出口不是目标请求出口的证明。未携带账号密钥时返回 401 视为目标可达；限流、验证和连接失败分别显示，不代表具体账号业务一定可用。</p>
 </template>
</Modal>
</template>
<style scoped>
.proxy-flags{display:flex;gap:6px;flex-wrap:wrap;margin-top:6px}.proxy-flags .badge{display:inline-flex;gap:4px;align-items:center}.proxy-actions{display:flex;gap:9px 13px;flex-wrap:wrap;min-width:150px;max-width:210px}.proxy-score{display:flex;align-items:baseline;gap:8px}.proxy-score b{font-size:20px}.proxy-testing{text-align:center;padding:30px 10px}.proxy-testing svg{color:var(--accent)}.proxy-testing p{color:var(--muted);font-size:12px}.proxy-result-heading{display:flex;gap:15px;align-items:center;margin:10px 0 24px}.proxy-result-heading>svg{color:#238169}.proxy-result-heading strong{font-size:34px}.proxy-result-heading strong small{font-size:14px}.proxy-result-heading small,.proxy-result-heading p{color:var(--muted)}.proxy-result-heading p{font-size:13px;margin:5px 0}.proxy-score-parts{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:20px}.proxy-score-parts>div{padding:12px 14px;background:#f4f8f8;border-radius:8px;display:flex;justify-content:space-between;font-size:12px}.proxy-sample{display:flex;flex-wrap:wrap;gap:12px;padding:9px 0;border-bottom:1px solid var(--line);font-size:12px}.proxy-sample small{margin-left:auto;color:var(--muted)}.callout small{color:var(--muted)}
</style>
