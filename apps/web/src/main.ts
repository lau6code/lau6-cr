import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'

import 'element-plus/dist/index.css'
// 暗色变量表：靠 <html class="dark"> 激活。
// ⚠️ 导入顺序要紧——本文件在它**之后**加载，所以 styles.css 里
//    html.dark 的定义最终生效（组件库的暗色值被我们的 token 接管）。
import 'element-plus/theme-chalk/dark/css-vars.css'
import './styles.css'

import App from './App.vue'
import { i18n } from './i18n'
import { router } from './router'

const app = createApp(App)

app.use(createPinia())
app.use(router)
app.use(i18n)
// 组件库的文案由 <el-config-provider> 按当前语言提供，
// 所以这里**不**固定 locale——固定了就切不动。
app.use(ElementPlus)

app.mount('#app')
