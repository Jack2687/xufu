<script setup lang="ts">
import {Check,Clock3,AlertTriangle} from 'lucide-vue-next'
import {computed} from 'vue'
const props=defineProps<{status:string;paymentState?:string;workflow?:string;renewal?:string}>()
const steps=computed(()=>props.workflow==='subscription'?['提交申请','排队准备','办理准备','订阅付款','取消自动续费','办理完成']:props.paymentState?['提交方案','排队准备','创建结账','付款处理','结果确认']:['提交方案','排队准备','创建结账','链接已生成'])
const current=()=>{
 if(props.workflow==='subscription'){
  if(props.status==='succeeded')return ['disabled','kept'].includes(props.renewal||'')?6:4
  if(props.status==='cancelled')return -1
  if(props.status==='queued')return 1
  if(props.paymentState==='succeeded')return 4
  return props.paymentState&&props.paymentState!=='preparing'?3:2
 }
 return props.paymentState?({preparing:3,quoted:3,submitting:3,checking:4,action_required:4,review:4,succeeded:5,failed:3,cancelled:3}[props.paymentState]??3):({draft:0,queued:1,processing:2,awaiting_payment:4,succeeded:4,failed:2,review:2,cancelled:-1}[props.status]??0)
}
</script>
<template><div class="progress-track" :style="{gridTemplateColumns:`repeat(${steps.length}, minmax(0, 1fr))`}"><div v-for="(s,i) in steps" :key="s" class="progress-step" :class="{done:i<current(),active:i===current()&&renewal!=='review',attention:workflow==='subscription'&&renewal==='review'&&i===4}"><span><AlertTriangle v-if="workflow==='subscription'&&renewal==='review'&&i===4" :size="15"/><Check v-else-if="i<current()" :size="15"/><Clock3 v-else-if="i===current()" :size="15"/><template v-else>{{i+1}}</template></span><b>{{workflow==='subscription'&&renewal==='review'&&i===4?'取消续费失败':s}}</b></div></div></template>
<style scoped>.progress-step.attention{color:#b45309}.progress-step.attention>span{background:#fff4df;border-color:#eab75a;color:#b45309}</style>
