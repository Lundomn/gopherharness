# 受控候选改进与人工激活

维护命令接收已审核的本地补丁，在固定 Git 基线上创建独立工作树，先比较训练用例，再用未参与挑选的验证用例验收。它不自动调用收费模型、不提交、不推送、不合并主分支。

## 固定评价契约

契约必须有训练用例、未见验证用例和允许修改路径。每条用例有独立 ID、命令和超时；两组 ID 与命令不能重复。例如下面是字段示意，测试名与路径必须替换为仓库中真实存在的内容：

```json
{
  "train": [
    {"id":"train-retry","command":"go test ./internal/example -run '^TestRetry$' -count=1","timeout_seconds":60}
  ],
  "holdout": [
    {"id":"holdout-cancel","command":"go test ./internal/example -run '^TestCancellation$' -count=1","timeout_seconds":60}
  ],
  "allowed_paths": ["internal/example/handler.go"],
  "protected_paths": ["go.mod", "go.sum"],
  "max_candidates": 3,
  "max_verifier_runs": 5,
  "timeout_seconds": 600
}
```

命令成功退出才算通过；超时、命令不可执行或不存在不作为一次有效的功能失败评分。用例必须真正验证产物，不要使用无断言的空命令。测试、测试数据、基准和工作流目录默认禁止候选修改，`protected_paths` 可加其他保护路径。

默认最多 3 个候选、总计 600 秒、单用例 60 秒；可配置上限分别为 20 个、3,600 秒、600 秒。`max_verifier_runs` 默认是“训练用例数 × (候选数 + 1) + 未见验证用例数”，包括基线；每次执行前先持久扣除次数。不是无限尝试直到碰巧通过。

## 运行与结果

```sh
./bin/gopherharness-maintain run --root /path/to/repo \
  --base HEAD --contract /path/to/cases.json \
  --patch /path/to/candidate-1.patch --patch /path/to/candidate-2.patch \
  --approve-verifiers --sandbox required
```

`--approve-verifiers` 明确授权执行契约命令；没有该参数就不创建维护任务。沙箱支持 `off / best_effort / required`，默认 off；Git 工作树隔离不等于系统安全隔离，不应在关闭沙箱时运行不可信校验脚本。

执行顺序：

1. 固定 Git 基线提交与契约摘要，单独运行一次训练基线；不会拷贝当前主工作区未提交的改动。
2. 每个候选使用独立编辑工作树，提取补丁，检查路径白名单、保护文件和文件类型，再应用到另一份干净训练工作树。
3. 候选必须比基线通过更多训练用例，且不能破坏基线已通过的任何用例。失败候选留下结果，不影响主工作区。
4. 按训练通过数选择唯一候选后冻结；在新的验证工作树中只运行一次未见用例集。全部通过才得到 `candidate_ready`。验证失败则拒绝整轮，不把失败详情反馈给候选生成器继续调参。
5. 运行结果、契约、补丁、工作树保留在 `.pico/maintenance/<run-id>/`。校验命令改动工作树、候选后来被修改或执行预算耗尽都会阻止激活。

Go 接口 `Evolver.Propose` 也可接自定义候选生成器；它只收到训练用例、基线与训练反馈，不收到未见用例。接口隔离不等于恶意生成器无法读取宿主文件，后者需要独立系统隔离。

## 查看、激活、回滚

```sh
# 查看某轮候选证据
./bin/gopherharness-maintain status --root /path/to/repo --run RUN_ID
# 查看当前激活版本，首次通常为 0
./bin/gopherharness-maintain status --root /path/to/repo
# expected-version 必须使用上一条命令实际返回的版本
./bin/gopherharness-maintain activate --root /path/to/repo \
  --run RUN_ID --expected-version 0 --approve-activation
# 下一次启动显式选择激活候选；不热替换正在运行的进程
./bin/gopherharness --cwd /path/to/repo --active-candidate "检查项目"
# 回滚示例：版本以当前 status 为准
./bin/gopherharness-maintain rollback --root /path/to/repo \
  --expected-version 1 --approve-activation
```

激活只切换 `.pico/maintenance/active.json` 内的本地工作树指针，不改主分支或部署。激活前复核补丁、验证结果和工作树内容；版本比较防止过期确认覆盖新选择。回滚重新校验上一候选；没有上一候选时回到原仓库。`active-root` 可供其他启动器取得并校验当前路径。

进程中断后不自动重跑结果未知的校验命令。检查该轮记录后发起新轮次；不能把 `running` 文件手改成 `candidate_ready`。当前是固定预算、固定划分、人工激活的有限闭环，不包含自动线上探索或统计显著性选优。
