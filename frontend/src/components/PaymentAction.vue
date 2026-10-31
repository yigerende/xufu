<script setup lang="ts">
import {onMounted,onUnmounted,ref} from 'vue'
import {api,type Row} from '../lib'
const props=defineProps<{orderId:string;canStart:boolean}>()
const emit=defineEmits<{refresh:[]}>()
const view=ref<Row>({state:'idle'}),open=ref(false),busy=ref(false),error=ref(''),text=ref('')
let timer:ReturnType<typeof setTimeout>|undefined,disposed=false,reading=false
const messages:Record<string,string>={available:'原付款验证入口已就绪，有效期最长十分钟',preparing:'正在打开官方验证页面',verifying:'请点击窗口内的“继续验证原付款”，并按官方提示操作',checking:'验证交互已结束，正在核实原交易',finished:'本次验证处理已结束，请查看原订单结果',closed:'验证窗口已关闭，原交易保留待核实'}
onMounted(async()=>{try{const current=await api(`/admin/orders/${props.orderId}/payment/action`);if(!disposed)view.value=current}catch(e){if(!disposed)error.value=(e as Error).message}})
async function poll(){
 if(disposed||!open.value||reading)return
 clearTimeout(timer);reading=true
 try{view.value=await api(`/admin/orders/${props.orderId}/payment/action`);if(['finished','closed'].includes(view.value.state)){emit('refresh');return}}catch(e){error.value=(e as Error).message}
 finally{reading=false;if(!disposed&&open.value&&!['finished','closed'].includes(view.value.state))timer=setTimeout(poll,2500)}
}
async function start(){
 if(busy.value)return;busy.value=true;error.value=''
 try{const current=await api(`/admin/orders/${props.orderId}/payment/action`);if(!['preparing','verifying','checking'].includes(current.state))await api(`/admin/orders/${props.orderId}/payment/action`,'POST',{});open.value=true;view.value={state:'preparing'};await poll();emit('refresh')}catch(e){error.value=(e as Error).message}finally{busy.value=false}
}
async function send(command:Row){
 if(!view.value.id)return
 try{await api(`/admin/orders/${props.orderId}/payment/action/input`,'POST',{id:view.value.id,...command})}catch(e){error.value=(e as Error).message}
}
function click(e:MouseEvent){(e.currentTarget as HTMLElement).parentElement?.focus();const box=(e.currentTarget as HTMLElement).getBoundingClientRect();void send({type:'click',x:(e.clientX-box.left)*960/box.width,y:(e.clientY-box.top)*720/box.height})}
function key(e:KeyboardEvent){if(['Tab','Enter','Escape','Backspace','Delete','ArrowUp','ArrowDown','ArrowLeft','ArrowRight',' '].includes(e.key)){e.preventDefault();void send({type:'key',key:e.key===' '?'Space':e.key})}else if(e.key.length===1&&!e.ctrlKey&&!e.metaKey){e.preventDefault();void send({type:'text',text:e.key})}}
async function enterText(){const value=text.value;text.value='';if(value)await send({type:'text',text:value})}
async function close(){await send({type:'close'});open.value=false;clearTimeout(timer);view.value={state:'closed'};emit('refresh')}
onUnmounted(()=>{disposed=true;clearTimeout(timer);text.value=''})
</script>
<template><section class="payment-action">
 <p class="alert danger" v-if="error" role="alert">{{error}}</p>
 <p v-if="!open&&view.state==='available'" role="status">{{messages.available}}。打开后在官方页面点击继续。</p>
 <button v-if="!open" type="button" class="btn primary" :disabled="busy||(!canStart&&view.state!=='available')" @click="start">{{busy?'准备中…':'继续原付款验证'}}</button>
 <div v-if="open" class="verification-window">
  <div class="verification-heading"><strong>官方付款验证</strong><button type="button" class="text-btn" @click="close">结束验证窗口</button></div>
  <p role="status">{{messages[view.state]||'正在读取验证状态'}}</p>
  <p class="small muted">点击窗口内的继续按钮后，官方 SDK 开始处理原交易；需要输入验证码或确认时，请按窗口提示操作。交互结束后会自动核实付款，并继续订阅及续费设置。</p>
  <div v-if="view.image" class="verification-screen" tabindex="0" aria-label="官方验证交互窗口" @keydown="key"><img :src="view.image" alt="原交易的官方验证页面" draggable="false" @click="click"/></div>
  <div v-if="view.image" class="verification-input"><input v-model="text" type="password" autocomplete="off" maxlength="128" placeholder="需要时输入验证内容" @keydown.enter.prevent="enterText"/><button type="button" class="btn secondary" @click="enterText">输入</button><button type="button" class="text-btn" @click="send({type:'key',key:'Tab'})">切换输入框</button><button type="button" class="text-btn" @click="send({type:'scroll',y:450})">向下滚动</button><button type="button" class="text-btn" @click="send({type:'scroll',y:-450})">向上滚动</button></div>
 </div>
</section></template>
<style scoped>
.payment-action{margin-top:16px}.verification-window{background:white;border:1px solid #c7dad7;border-radius:12px;padding:16px}.verification-heading{display:flex;justify-content:space-between;gap:12px}.verification-window p{line-height:1.7;font-size:13px}.verification-screen{width:100%;background:#f4f7fc;outline-offset:3px}.verification-screen img{display:block;width:100%;height:auto;cursor:pointer}.verification-input{display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-top:12px}.verification-input input{flex:1;min-width:140px}
</style>
