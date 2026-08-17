# 模型映射编辑器 Lint 基线问题记录

## 背景

在对模型映射编辑器（`web/src/features/channels/components/model-mapping-editor.tsx`）做「原始模型候选三组下拉」改造时，通过 `bunx oxlint -c .oxlintrc.json <file>` 检查发现 3 个 error 和 1 个 warning。

经 `git stash` 对比验证：**以上 4 项均为 main 分支既有问题，不是本次改造引入的**（改造仅新增了 `GroupedComboboxInput` 组件，未触碰以下代码段）。

本记录用于留存基线，供后续按需清理。若清理，改动会落在 `model-mapping-editor.tsx`，属独立提交。

## 3 个 Error

### 1. unicorn/prefer-spread —— 第 328 行

```ts
return Array.from(duplicates)
```

- 位置：`getDuplicateSources` 函数返回值。
- 建议：优先使用展开运算符 `[...duplicates]` 而非 `Array.from()`。

### 2. unicorn/prefer-optional-catch-binding —— 第 396 行

```ts
} catch (_error) {
```

- 位置：`parseJsonToRows` 的 JSON 解析兜底 catch。
- 建议：catch 绑定参数未被使用，应省略为 `catch {`。

### 3. react-hooks/exhaustive-deps —— 第 407 行

```ts
useEffect(() => {
  setJsonValue(props.value)
  parseJsonToRows(props.value)
}, [props.value])
```

- 位置：外部 value 变化时重新解析 JSON 的 effect。
- 建议：依赖数组中缺少 `parseJsonToRows`（函数每次渲染重新创建，直接加入会引发循环，需配合 `useCallback` 处理）。

## 1 个 Warning

### 4. react/self-closing-comp —— 第 543 行

```tsx
<div className='w-10'></div>
```

- 位置：映射表格表头占位单元格。
- 建议：改为自闭合 `<div className='w-10' />`。

## 校验方式

```bash
cd web
bunx oxlint -c .oxlintrc.json src/features/channels/components/model-mapping-editor.tsx
```

预期输出：`Found 1 warning and 3 errors.`

> 说明：`tsc`、`vitest` 不受以上 lint 问题影响，均正常通过（31 个测试文件 / 156 个用例）。
