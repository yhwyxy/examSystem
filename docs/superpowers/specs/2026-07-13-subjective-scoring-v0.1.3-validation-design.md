# subjective-scoring v0.1.3 同卷验证设计

## 目标

将 `examSystem` 的 `subjective-scoring` 依赖从 `v0.1.2` 更新到 annotated tag `v0.1.3`，验证 tag 解引用后指向提交 `2cf6b24`，并使用现有文本、SQL、代码专项卷及相同答案重新评分，与 v0.1.2 基线直接比较。

## 依赖更新

以下位置统一固定为 `v0.1.3`：

- `pyproject.toml` 的 uv Git source
- `requirements.txt` 的 Git URL
- `uv.lock` 的 Git tag 与提交 revision
- README 的当前版本和安装示例
- `tests/test_dependency_boundaries.py` 的版本断言

更新前通过远端 tag 查询确认 `v0.1.3^{}` 为 `2cf6b24`。更新后安装包元数据必须报告版本 `0.1.3`。

## 验证数据

复用以下试卷，不修改题目、参考答案、评分点或答案：

- `text-scoring-specialist`
- `sql-scoring-specialist`
- `code-scoring-specialist`

每套试卷继续使用 `complete`、`partial`、`wrong` 三档固定答案。验证脚本在临时目录创建试卷和数据库，正式 `data/papers/` 与生产数据库必须保持不变。

## 对比基线

v0.1.2 基线为 `reports/scoring-validation-20260712-210421.json`。新报告除现有工作流和评分指标外，还应形成版本对比摘要，至少包含：

- 三套专项卷各档总分的前后变化
- 完整答案是否保持高于部分答案和错误答案
- 错误答案是否更接近 0 分
- 工作流成功率、评分错误数和排序准确率变化
- 得分变化最大的具体题目

## 成功标准

1. `v0.1.3` tag 解引用提交与 `2cf6b24` 一致。
2. 依赖声明、锁文件和实际安装版本一致。
3. 16 个固定提交全部完成，无评分引擎错误。
4. 三套专项卷均保持 `complete > partial > wrong`。
5. 完整测试套件通过；若新版公开契约导致旧断言失效，必须核对 v0.1.3 源码或上游测试后再更新断言。

## 范围限制

本次不修改试卷内容、不调整评分参数、不改变业务评分规则，也不清理历史文件。发现的分数问题只记录和分析，不在同一轮中修正评分算法。
