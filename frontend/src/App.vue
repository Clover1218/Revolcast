<script setup lang="ts">
import { ref } from 'vue'

const title = ref('Revolcast')
</script>

<template>
  <div
    class="min-h-screen bg-gray-950 text-neon flex flex-col items-center justify-center gap-4"
  >
    <h1 class="text-4xl font-bold tracking-widest">
      {{ title }} <span class="text-neon-cyan">控制台</span>
    </h1>
    <p class="text-gray-400">实时消息分发与自动化控制中枢</p>
    <div class="px-4 py-2 border border-neon/40 rounded-lg">
      ● 系统初始化完成
    </div>
  </div>
</template>
