/* 语言包登记处
 *
 * 加一门语言只做三件事：
 *   1. 新建 i18n/<code>.js，默认导出 { '中文原文': '译文' }
 *   2. 在这里 import 并登记（键必须是 LANGS 里的 code）
 *   3. 在 core.js 的 LANGS 里补 { code, label, short }
 * 没翻到的文案会自动回退中文，所以语言包可以先上一半再慢慢补。
 */
import en from './en.js';

export default {
  en,
};
