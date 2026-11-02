import zhCN from './zh-CN'

/**
 * 把字面量类型放宽成 string。
 *
 * 语言包之间的**文案**本来就不同，但**键结构**必须完全一致——
 * 少一个键就是漏翻译，多一个键就是打错字。
 */
type Widen<T> = T extends string ? string : { [K in keyof T]: Widen<T[K]> }

/** 语言包的结构类型。 */
export type Messages = Widen<typeof zhCN>
