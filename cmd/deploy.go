package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/dustin/go-humanize"
	"github.com/logrusorgru/aurora"
	"github.com/mattn/go-isatty"
	"github.com/morikuni/aec"
	"github.com/spf13/cobra"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/cmd/presenters"
	"github.com/superfly/flyctl/cmdctx"
	"github.com/superfly/flyctl/docker"
	"github.com/superfly/flyctl/docstrings"
	"github.com/superfly/flyctl/internal/builds"
	"github.com/superfly/flyctl/internal/deployment"
	"github.com/superfly/flyctl/terminal"
)

func newDeployCommand() *Command {
	deployStrings := docstrings.Get("deploy")
	cmd := BuildCommand(nil, runDeploy, deployStrings.Usage, deployStrings.Short, deployStrings.Long, os.Stdout, workingDirectoryFromArg(0), requireSession, requireAppName)
	cmd.AddStringFlag(StringFlagOpts{
		Name:        "image",
		Shorthand:   "i",
		Description: "Image tag or id to deploy",
	})
	cmd.AddBoolFlag(BoolFlagOpts{
		Name:        "detach",
		Description: "Return immediately instead of monitoring deployment progress",
	})
	cmd.AddBoolFlag(BoolFlagOpts{
		Name:   "build-only",
		Hidden: true,
	})
	cmd.AddBoolFlag(BoolFlagOpts{
		Name:        "remote-only",
		Description: "Perform builds remotely without using the local docker daemon",
	})
	cmd.AddStringFlag(StringFlagOpts{
		Name:        "strategy",
		Description: "The strategy for replacing running instances. Options are canary, rolling, or immediate. Default is canary",
	})
	cmd.AddStringFlag(StringFlagOpts{
		Name:        "dockerfile",
		Description: "Path to a Dockerfile. Defaults to the Dockerfile in the working directory.",
	})
	cmd.AddStringSliceFlag(StringSliceFlagOpts{
		Name:        "build-arg",
		Description: "Set of build time variables in the form of NAME=VALUE pairs. Can be specified multiple times.",
	})
	cmd.AddStringFlag(StringFlagOpts{
		Name:        "image-label",
		Description: "Image label to use when tagging and pushing to the fly registry. Defaults to \"deployment-{timestamp}\".",
	})

	cmd.Command.Args = cobra.MaximumNArgs(1)

	return cmd
}

func runDeploy(commandContext *cmdctx.CmdContext) error {
	ctx := createCancellableContext()
	op, err := docker.NewDeployOperation(ctx, commandContext)
	if err != nil {
		return err
	}

	commandContext.Status("flyctl", cmdctx.STITLE, "Deploying", commandContext.AppName)

	commandContext.Status("flyctl", cmdctx.SBEGIN, "Validating App Configuration")
	parsedCfg, err := op.ValidateConfig()
	if err != nil {
		for _, error := range parsedCfg.Errors {
			//	fmt.Println("   ", aurora.Red("✘").String(), error)
			commandContext.Status("flyctl", cmdctx.SERROR, "   ", aurora.Red("✘").String(), error)
		}
		return err
	}
	commandContext.Status("flyctl", cmdctx.SDONE, "Validating App Configuration done")

	if parsedCfg.Valid {
		if len(parsedCfg.Services) > 0 {
			commandContext.Frender(cmdctx.PresenterOption{Presentable: &presenters.SimpleServices{Services: parsedCfg.Services}, HideHeader: true, Vertical: false, Title: "Services"})
		}
	}

	appcheck, err := commandContext.Client.API().GetApp(commandContext.AppName)

	if err != nil {
		return err
	}

	if appcheck.Status == "dead" {
		return fmt.Errorf("app %s is currently paused - resume it with flyctl apps resume", commandContext.AppName)
	}

	var strategy = docker.DefaultDeploymentStrategy
	if val, _ := commandContext.Config.GetString("strategy"); val != "" {
		strategy, err = docker.ParseDeploymentStrategy(val)
		if err != nil {
			return err
		}
	}

	var image *docker.Image

	if imageRef, _ := commandContext.Config.GetString("image"); imageRef != "" {
		// image specified, resolve it, tagging and pushing if docker+local

		commandContext.Statusf("flyctl", cmdctx.SINFO, "Deploying image: %s\n", imageRef)

		img, err := op.ResolveImage(ctx, commandContext, imageRef)
		if err != nil {
			return err
		}
		image = img
	} else {
		// no image specified, build one
		buildArgs := map[string]string{}
		for _, arg := range commandContext.Config.GetStringSlice("build-arg") {
			parts := strings.Split(arg, "=")
			if len(parts) != 2 {
				return fmt.Errorf("Invalid build-arg '%s': must be in the format NAME=VALUE", arg)
			}
			buildArgs[parts[0]] = parts[1]
		}

		var dockerfilePath string

		if dockerfile, _ := commandContext.Config.GetString("dockerfile"); dockerfile != "" {
			dockerfilePath = dockerfile
		}

		if dockerfilePath == "" {
			dockerfilePath = docker.ResolveDockerfile(commandContext.WorkingDir)
		}

		if dockerfilePath == "" && !commandContext.AppConfig.HasBuilder() {
			return docker.ErrNoDockerfile
		}

		if commandContext.AppConfig.HasBuilder() {
			if dockerfilePath != "" {
				terminal.Warn("Project contains both a Dockerfile and buildpacks, using buildpacks")
			}
		}

		//fmt.Printf("Deploy source directory '%s'\n", cc.WorkingDir)
		commandContext.Statusf("flyctl", cmdctx.SINFO, "Deploy source directory '%s'\n", commandContext.WorkingDir)

		if op.DockerAvailable() {
			//fmt.Println("Docker daemon available, performing local build...")
			commandContext.Status("flyctl", cmdctx.SDETAIL, "Docker daemon available, performing local build...")

			if commandContext.AppConfig.HasBuilder() {
				//fmt.Println("Building with buildpacks")
				commandContext.Status("flyctl", cmdctx.SBEGIN, "Building with buildpacks")
				img, err := op.BuildWithPack(commandContext, buildArgs)
				if err != nil {
					return err
				}
				image = img
				commandContext.Status("flyctl", cmdctx.SDONE, "Building with buildpacks done")
			} else {
				//fmt.Println("Building Dockerfile")
				commandContext.Status("flyctl", cmdctx.SBEGIN, "Building with Dockerfile")

				img, err := op.BuildWithDocker(commandContext, dockerfilePath, buildArgs)
				if err != nil {
					return err
				}
				image = img
				commandContext.Status("flyctl", cmdctx.SDONE, "Building with Dockerfile done")
			}
			commandContext.Statusf("flyctl", cmdctx.SINFO, "Image: %+v\n", image.Tag)
			commandContext.Statusf("flyctl", cmdctx.SINFO, "Image size: %s\n", humanize.Bytes(uint64(image.Size)))

			commandContext.Status("flyctl", cmdctx.SBEGIN, "Pushing Image")
			err := op.PushImage(*image)
			if err != nil {
				return err
			}
			commandContext.Status("flyctl", cmdctx.SDONE, "Done Pushing Image")

			if commandContext.Config.GetBool("build-only") {
				commandContext.Statusf("flyctl", cmdctx.SINFO, "Image: %s\n", image.Tag)

				return nil
			}

		} else {
			//fmt.Println("Docker daemon unavailable, performing remote build...")
			commandContext.Status("flyctl", cmdctx.SINFO, "Docker daemon unavailable, performing remote build...")

			build, err := op.StartRemoteBuild(commandContext.WorkingDir, commandContext.AppConfig, dockerfilePath, buildArgs)
			if err != nil {
				return err
			}

			s := spinner.New(spinner.CharSets[11], 100*time.Millisecond)
			s.Writer = os.Stderr
			s.Prefix = "Building "
			s.Start()

			buildMonitor := builds.NewBuildMonitor(build.ID, commandContext.Client.API())
			for line := range buildMonitor.Logs(ctx) {
				s.Stop()
				commandContext.Status("remotebuild", cmdctx.SINFO, line)
				s.Start()
			}

			s.FinalMSG = fmt.Sprintf("Build complete - %s\n", buildMonitor.Status())
			s.Stop()

			if err := buildMonitor.Err(); err != nil {
				return err
			}
			if buildMonitor.Failed() {
				return errors.New("build failed")
			}

			build = buildMonitor.Build()
			image = &docker.Image{
				Tag: build.Image,
			}
		}
	}

	if image == nil {
		return errors.New("Could not find an image to deploy")
	}

	commandContext.Status("flyctl", cmdctx.SBEGIN, "Optimizing Image")
	if err := op.OptimizeImage(*image); err != nil {
		return err
	}
	commandContext.Status("flyctl", cmdctx.SDONE, "Done Optimizing Image")

	commandContext.Status("flyctl", cmdctx.SBEGIN, "Creating Release")

	if strategy != docker.DefaultDeploymentStrategy {
		commandContext.Statusf("flyctl", cmdctx.SDETAIL, "Deployment Strategy: %s", strategy)
	}

	release, err := op.Deploy(*image, strategy)
	if err != nil {
		return err
	}

	op.CleanDeploymentTags()

	commandContext.Statusf("flyctl", cmdctx.SINFO, "Release v%d created\n", release.Version)

	if strings.ToLower(release.DeploymentStrategy) == string(docker.ImmediateDeploymentStrategy) {
		return nil
	}

	return watchDeployment(ctx, commandContext)
}

func watchDeployment(ctx context.Context, commandContext *cmdctx.CmdContext) error {
	if commandContext.Config.GetBool("detach") {
		return nil
	}

	commandContext.Status("deploy", cmdctx.STITLE, "Monitoring Deployment")
	commandContext.Status("deploy", cmdctx.SDETAIL, "You can detach the terminal anytime without stopping the deployment")

	interactive := isatty.IsTerminal(os.Stdout.Fd())

	monitor := deployment.NewDeploymentMonitor(commandContext.Client.API(), commandContext.AppName)
	monitor.DeploymentStarted = func(idx int, d *api.DeploymentStatus) error {
		if idx > 0 {
			commandContext.StatusLn()
		}
		commandContext.Status("deploy", cmdctx.SINFO, presenters.FormatDeploymentSummary(d))
		return nil
	}
	monitor.DeploymentUpdated = func(d *api.DeploymentStatus, updatedAllocs []*api.AllocationStatus) error {
		if interactive && !commandContext.OutputJSON() {
			fmt.Fprint(commandContext.Out, aec.Up(1))
			fmt.Fprint(commandContext.Out, aec.EraseLine(aec.EraseModes.All))
			fmt.Fprintln(commandContext.Out, presenters.FormatDeploymentAllocSummary(d))
		} else {
			for _, alloc := range updatedAllocs {
				commandContext.Status("deploy", cmdctx.SINFO, presenters.FormatAllocSummary(alloc))
			}
		}
		return nil
	}
	monitor.DeploymentFailed = func(d *api.DeploymentStatus, failedAllocs []*api.AllocationStatus) error {
		commandContext.Statusf("deploy", cmdctx.SDETAIL, "v%d %s - %s\n", d.Version, d.Status, d.Description)

		if len(failedAllocs) > 0 {
			commandContext.Status("flyctl", cmdctx.STITLE, "Failed Allocations")

			x := make(chan *api.AllocationStatus)
			var wg sync.WaitGroup
			wg.Add(len(failedAllocs))

			for _, a := range failedAllocs {
				a := a
				go func() {
					defer wg.Done()
					alloc, err := commandContext.Client.API().GetAllocationStatus(commandContext.AppName, a.ID, 20)
					if err != nil {
						commandContext.Status("flyctl", cmdctx.SERROR, "Error fetching alloc", a.ID, err)
						return
					}
					x <- alloc
				}()
			}

			go func() {
				wg.Wait()
				close(x)
			}()

			count := 0
			for alloc := range x {
				count++
				commandContext.Statusf("flyctl", cmdctx.SERROR, "\n  Failure #%d\n", count)
				err := commandContext.Frender(
					cmdctx.PresenterOption{
						Title: "Allocation",
						Presentable: &presenters.Allocations{
							Allocations: []*api.AllocationStatus{alloc},
						},
						Vertical: true,
					},
					cmdctx.PresenterOption{
						Title: "Recent Events",
						Presentable: &presenters.AllocationEvents{
							Events: alloc.Events,
						},
					},
				)
				if err != nil {
					return err
				}

				commandContext.Status("flyctl", cmdctx.STITLE, "Recent Logs")
				logPresenter := presenters.LogPresenter{HideAllocID: true, HideRegion: true, RemoveNewlines: true}
				logPresenter.FPrint(commandContext.Out, commandContext.OutputJSON(), alloc.RecentLogs)
			}

		}
		return nil
	}
	monitor.DeploymentSucceeded = func(d *api.DeploymentStatus) error {
		commandContext.Statusf("flyctl", cmdctx.SDONE, "v%d deployed successfully\n", d.Version)
		return nil
	}

	monitor.Start(ctx)

	if err := monitor.Error(); err != nil {
		return err
	}

	if !monitor.Success() {
		return ErrAbort
	}

	return nil
}
