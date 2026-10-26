package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/Lundomn/gopherharness/internal/maintenance"
	"github.com/Lundomn/gopherharness/internal/sandbox"
)

type patches []string

func (p *patches) String() string         { return strings.Join(*p, ",") }
func (p *patches) Set(value string) error { *p = append(*p, value); return nil }
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("使用 run / status / activate / rollback / active-root")
	}
	fs := flag.NewFlagSet("gopherharness-maintain "+args[0], flag.ContinueOnError)
	root := fs.String("root", ".", "Git 仓库根目录")
	id := fs.String("run", "", "维护运行 ID")
	base := fs.String("base", "HEAD", "固定 Git 基线")
	contractPath := fs.String("contract", "", "包含固定训练和未见验证用例的 JSON")
	request := fs.String("request", "改进本地候选", "候选生成目标")
	approveVerify := fs.Bool("approve-verifiers", false, "明确允许执行契约中的本地命令")
	approveActivate := fs.Bool("approve-activation", false, "明确允许切换本地候选指针")
	version := fs.Uint64("expected-version", 0, "status 显示的激活版本，防止旧确认覆盖新选择")
	sandboxMode := fs.String("sandbox", "off", "off / best_effort / required；off 不是安全沙箱")
	var inputs patches
	fs.Var(&inputs, "patch", "已审核的本地候选补丁，可重复")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("不接受位置参数")
	}
	var output any
	switch args[0] {
	case "run":
		if !*approveVerify {
			return errors.New("需要 --approve-verifiers 才能运行外部校验命令")
		}
		if len(inputs) == 0 || *contractPath == "" {
			return errors.New("需要 --contract 和至少一个 --patch")
		}
		contract, err := maintenance.ReadContract(*contractPath)
		if err != nil {
			return err
		}
		if len(inputs) > contract.MaxCandidates {
			return errors.New("候选补丁数超过契约 max_candidates")
		}
		e := maintenance.Evolver{Config: maintenance.Config{Root: *root, BaseRevision: *base, ApproveVerifiers: true, Sandbox: sandbox.Config{Mode: *sandboxMode, WorkspaceWrite: true}}, Propose: maintenance.PatchProposal(inputs)}
		result, err := e.Run(ctx, *request, contract)
		output = result
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(result)
			return err
		}
	case "status":
		if *id != "" {
			v, err := maintenance.ReadEvolution(*root, *id)
			if err != nil {
				return err
			}
			output = v
		} else {
			v, err := maintenance.Active(ctx, *root)
			if err != nil {
				return err
			}
			output = v
		}
	case "activate":
		v, err := maintenance.Activate(ctx, *root, *id, *version, *approveActivate)
		if err != nil {
			return err
		}
		output = v
	case "rollback":
		v, err := maintenance.Rollback(ctx, *root, *version, *approveActivate)
		if err != nil {
			return err
		}
		output = v
	case "active-root":
		v, err := maintenance.ResolveActiveRoot(ctx, *root)
		if err != nil {
			return err
		}
		output = map[string]string{"root": v}
	default:
		return fmt.Errorf("未知子命令 %q", args[0])
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
