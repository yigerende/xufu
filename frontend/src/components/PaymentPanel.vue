<script setup lang="ts">
import {computed,onUnmounted,ref} from 'vue'
import {CreditCard,ShieldCheck,RefreshCw} from 'lucide-vue-next'
import {api,cash,date,notify,type Row} from '../lib'
import PaymentAction from './PaymentAction.vue'
const props=defineProps<{order:Row;payment?:Row;checkout?:Row;admin?:boolean}>()
const emit=defineEmits<{refresh:[]}>()
const closing=ref(false),closeReason=ref(''),closeAccepted=ref(false)
const canClose=computed(()=>props.admin&&props.payment?.fee_state==='held'&&!['succeeded','failed','cancelled'].includes(props.payment?.state)&&props.order.status!=='succeeded')
async function closeOrder(){await run(async()=>{await api(`/admin/orders/${props.order.id}/close`,'POST',{reason:closeReason.value,accepted:closeAccepted.value});closing.value=false;closeReason.value='';closeAccepted.value=false;notify('订单已关闭，冻结服务费已释放')})}
const number=ref(''),month=ref(''),year=ref(''),cvc=ref(''),accepted=ref(false),agreeQuote=ref(false),busy=ref(false),error=ref('')
const states:Row={preparing:'准备付款',quoted:'等待金额确认',submitting:'正在付款',checking:'核实付款结果',action_required:'需要额外验证',review:'结果待核实',succeeded:'付款成功',failed:'准备未完成',cancelled:'准备已取消'}
const canPrepare=computed(()=>!props.admin&&props.order.workflow!=='subscription'&&props.payment?.enabled&&props.order.status==='awaiting_payment'&&(!props.payment.state||['failed','cancelled'].includes(props.payment.state)))
const amount=computed(()=>{const p=props.payment;if(p?.amount==null||!p.currency)return '—';const digits=p.currency.toUpperCase()==='JPY'?0:2;return new Intl.NumberFormat('zh-CN',{style:'currency',currency:p.currency.toUpperCase(),minimumFractionDigits:digits}).format(Number(p.amount)/(digits?100:1))})
function clear(){number.value='';month.value='';year.value='';cvc.value='';accepted.value=false}
onUnmounted(clear)
async function run(fn:()=>Promise<void>){if(busy.value)return;busy.value=true;error.value='';try{await fn();emit('refresh')}catch(e){error.value=(e as Error).message}finally{busy.value=false}}
async function prepare(){await run(async()=>{try{await api(`/orders/${props.order.id}/payment/prepare`,'POST',{card:{number:number.value,month:month.value,year:year.value,cvc:cvc.value},accepted:accepted.value});notify('正在准备含税报价，尚未提交扣款')}finally{clear()}})}
async function confirm(){await run(async()=>{await api(`/orders/${props.order.id}/payment/confirm`,'POST',{attempt_id:props.payment?.attempt_id,amount:props.payment?.amount,currency:props.payment?.currency,accepted:agreeQuote.value});agreeQuote.value=false})}
async function check(){await run(async()=>{await api(`${props.admin?'/admin':''}/orders/${props.order.id}/payment/check`,'POST',{})})}
</script>
<template><section class="payment-panel" v-if="payment?.state||canPrepare">
<div class="panel-heading"><h3><CreditCard :size="19"/>站内付款</h3><span v-if="payment?.state" class="badge" :class="payment.state">{{payment.error_code==='admin_closed'?'人工关闭':states[payment.state]||'处理中'}}</span></div>
<div class="alert danger" v-if="error" role="alert">{{error}}</div>
<p v-if="payment?.message" role="status">{{payment.message}}</p>
<PaymentAction v-if="admin&&order.subscription_flow==='cslive'&&!payment?.automatic_running&&order.workflow==='subscription'&&['action_required','checking'].includes(payment?.state)" :key="order.id" :order-id="order.id" :can-start="!!payment?.can_check||payment?.state==='checking'" @refresh="emit('refresh')"/>
<div v-if="canClose" class="manual-close"><button v-if="!closing" type="button" class="btn secondary" @click="closing=true">人工关闭订单并释放服务费</button><div v-else class="manual-close-confirm"><p><b>停止本站后续办理，并将 {{cash(order.fee)}} R 冻结服务费退回客户可用余额。</b></p><p>已经发往银行的付款不会因此撤销，原交易仍可能完成扣款；关闭后不会自动核实、续办或取消自动续费，请另行核对原交易。</p><label>关闭原因<textarea v-model="closeReason" required minlength="2" maxlength="300" placeholder="填写业务关闭原因，不要填写密钥、卡号或验证码"/></label><label class="checkbox-label"><input type="checkbox" v-model="closeAccepted" required/>我确认停止办理并释放冻结服务费，已了解此操作不退款或撤销银行卡付款。</label><div class="actions"><button type="button" class="btn primary" :disabled="busy||!closeAccepted||closeReason.trim().length<2" @click="closeOrder">确认关闭并释放</button><button type="button" class="btn secondary" :disabled="busy" @click="closing=false">返回</button></div></div></div>
<div v-if="payment?.state" class="payment-summary"><span>付款卡尾号 <b>{{payment.last4||'—'}}</b></span><span>含税订阅金额 <b>{{amount}}</b></span><span>服务费 <b>{{cash(order.fee)}} R</b> · {{({held:'已冻结',charged:'已结算',released:'已解冻'} as Row)[payment.fee_state]}}</span></div>
<p v-if="payment?.charged_amount!=null" class="small muted">已核实实收：{{payment.charged_amount/(payment.charged_currency.toUpperCase()==='JPY'?1:100)}} {{payment.charged_currency.toUpperCase()}}</p><p v-if="order.activation==='review'&&payment?.state==='succeeded'" class="callout">付款已确认，订阅生效尚待核实。请联系运营处理，无需再次付款。</p><form v-if="canPrepare" @submit.prevent="prepare" autocomplete="off">
<p>填写本人或获授权的真实银行卡。先核对含税报价，再确认付款；服务费 {{cash(order.fee)}} R 将暂时冻结。</p>
<label>卡号<input v-model="number" inputmode="numeric" autocomplete="off" required maxlength="23" placeholder="银行卡卡号"/></label>
<div class="form-grid payment-card-fields"><label>有效月<input v-model="month" inputmode="numeric" required pattern="[0-9]{1,2}" maxlength="2" placeholder="MM"/></label><label>有效年<input v-model="year" inputmode="numeric" required pattern="[0-9]{2,4}" maxlength="4" placeholder="YYYY"/></label><label>安全码<input v-model="cvc" type="password" inputmode="numeric" autocomplete="off" required pattern="[0-9]{3,4}" maxlength="4" placeholder="CVV"/></label></div>
<label class="checkbox-label"><input type="checkbox" v-model="accepted" required/>我有权使用此卡，理解最终订阅金额和服务费分别结算。</label>
<button class="btn primary" :disabled="busy||!accepted"><ShieldCheck :size="16"/>{{busy?'提交中…':'准备报价'}}</button><small class="muted">卡号和安全码仅用于本次处理，不保存到订单或操作日志。</small>
</form>
<div v-if="payment?.state==='quoted'"><p>本次银行卡扣款：<strong>{{amount}}</strong>，另结算服务费 {{cash(order.fee)}} R。报价有效至 {{date(payment.quote_expires_at)}}。</p><template v-if="!admin&&order.workflow!=='subscription'"><label class="checkbox-label"><input type="checkbox" v-model="agreeQuote"/>我确认以上金额，同意提交付款。</label><button class="btn primary" :disabled="busy||!agreeQuote" @click="confirm">确认付款 {{amount}}</button></template></div>
<div class="actions" v-if="!payment?.automatic_running&&(['review','action_required'].includes(payment?.state)||payment?.can_check)"><button class="btn secondary" :disabled="busy||payment?.can_check===false" @click="check"><RefreshCw :size="16"/>{{payment?.state==='succeeded'?'继续核实订阅状态':'核实付款结果'}}</button></div>
<p class="small muted" v-if="order.workflow!=='subscription'&&['submitting','review','checking','action_required'].includes(payment?.state)">处理中请勿在其他页面再次付款。需要额外验证时，请使用上方官方付款页完成；该页面使用你的浏览器网络，完成后点击核实结果。</p>
<p v-if="order.workflow==='subscription'&&payment?.state==='succeeded'">订阅核实：{{({pending:'已付款，等待订阅同步',active:'订阅已生效',review:'已付款，订阅生效待核实',not_checked:'历史订单未单独记录'} as Row)[order.activation]||'等待核实'}}</p><p v-if="order.workflow==='subscription'">续费设置：{{!order.keep_renewal&&['pending','review'].includes(order.activation)&&payment?.state==='succeeded'?'待订阅生效核实后处理':({kept:'已选择保留自动续费',pending:'付款后自动关闭续费',disabled:'已关闭自动续费',review:'取消自动续费失败',retrying:'正在重新处理续费',not_started:'尚未开始'} as Row)[order.renewal]||'等待处理'}}</p><div v-if="order.workflow==='subscription'&&order.renewal==='review'&&payment?.state==='succeeded'&&order.activation!=='review'" class="callout"><div><b>订阅成功，取消自动续费失败</b><p>付款已确认，无需重新订阅。请前往续费管理，粘贴同一账号的新 Session 继续关闭；本次操作不收费。</p><a href="#/renewal" class="btn secondary" :target="admin?'_blank':undefined" rel="noopener noreferrer">去关闭自动续费</a></div></div><p class="small muted" v-if="order.workflow==='subscription'&&!payment?.automatic_running&&payment?.state==='action_required'">付款等待官方验证，流程已暂停，请联系运营核实。</p></section></template>
<style scoped>
.payment-panel{border:1px solid #b7d9d1;background:#f4faf8;border-radius:16px;padding:24px;margin:20px 0}.payment-panel h3{display:flex;gap:8px;align-items:center}.payment-summary{display:flex;flex-wrap:wrap;gap:12px 24px;border-top:1px solid #d9e8e3;border-bottom:1px solid #d9e8e3;padding:14px 0;margin:16px 0;font-size:13px}.payment-card-fields{grid-template-columns:repeat(3,1fr)}.payment-panel form>small{display:block;margin-top:12px}.payment-panel .actions{margin-top:16px}
</style>
