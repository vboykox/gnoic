package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
	"github.com/openconfig/gnoi/file"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/prototext"
)

type fileStatResponse struct {
	TargetError
	rsp []*fileStatInfo
}

type fileStatInfo struct {
	si    *file.StatInfo
	isDir bool
}

func (a *App) InitFileStatFlags(cmd *cobra.Command) {
	cmd.ResetFlags()
	//
	cmd.Flags().StringSliceVar(&a.Config.FileStatPath, "path", []string{}, "path(s) to get metadata about")
	cmd.Flags().BoolVar(&a.Config.FileStatHumanize, "humanize", false, "make outputted values human readable")
	cmd.Flags().BoolVar(&a.Config.FileStatRecursive, "recursive", false, "recursively lookup subdirectories")
	//
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		a.Config.FileConfig.BindPFlag(fmt.Sprintf("%s-%s", cmd.Name(), flag.Name), flag)
	})
}

func (a *App) RunEFileStat(cmd *cobra.Command, args []string) error {
	targets, err := a.GetTargets()
	if err != nil {
		return err
	}

	numTargets := len(targets)
	responseChan := make(chan *fileStatResponse, numTargets)

	a.wg.Add(numTargets)
	for _, t := range targets {
		go func(t *Target) {
			defer a.wg.Done()
			ctx, cancel := context.WithCancel(a.ctx)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, "username", *t.Config.Username, "password", *t.Config.Password)

			err = a.CreateGrpcClient(ctx, t, a.createBaseDialOpts()...)
			if err != nil {
				responseChan <- &fileStatResponse{
					TargetError: TargetError{
						TargetName: t.Config.Address,
						Err:        err,
					},
				}
				return
			}
			rsp, err := a.FileStat(ctx, t)
			responseChan <- &fileStatResponse{
				TargetError: TargetError{
					TargetName: t.Config.Address,
					Err:        err,
				},
				rsp: rsp,
			}
		}(t)
	}
	a.wg.Wait()
	close(responseChan)

	errs := make([]error, 0, numTargets)
	result := make([]*fileStatResponse, 0, numTargets)
	for rsp := range responseChan {
		if rsp.Err != nil {
			wErr := fmt.Errorf("%q File Stat failed: %v", rsp.TargetName, rsp.Err)
			a.Logger.Error(wErr)
			errs = append(errs, wErr)
			continue
		}
		result = append(result, rsp)
	}

	fmt.Print(a.statTable(result))
	return a.handleErrs(errs)
}

func (a *App) FileStat(ctx context.Context, t *Target) ([]*fileStatInfo, error) {
	fileClient := file.NewFileClient(t.client)
	rsps := make([]*fileStatInfo, 0, len(a.Config.FileStatPath))
	for _, path := range a.Config.FileStatPath {
		fsi, err := a.fileStat(ctx, t, fileClient, path)
		if err != nil {
			return nil, err
		}
		rsps = append(rsps, fsi...)
	}
	return rsps, nil
}

func (a *App) fileStat(ctx context.Context, t *Target, fileClient file.FileClient, path string) ([]*fileStatInfo, error) {
	r, err := fileClient.Stat(ctx, &file.StatRequest{
		Path: path,
	})
	if err != nil {
		return nil, fmt.Errorf("%q file %q stat err: %v", t.Config.Address, path, err)
	}
	a.Logger.Debugf("%q File Stat Response:\n%s", t.Config.Address, prototext.Format(r))
	rsps := make([]*fileStatInfo, 0, len(r.Stats))
	for _, si := range r.Stats {
		isDir, err := a.isDir(ctx, fileClient, si.Path)
		if err != nil {
			a.Logger.Errorf("%q file %q isDir err: %v", t.Config.Address, path, err)
			continue
		}

		rsps = append(rsps, &fileStatInfo{
			si:    si,
			isDir: isDir,
		})

		if isDir && a.Config.FileStatRecursive {
			fsi, err := a.fileStat(ctx, t, fileClient, si.Path)
			if err != nil {
				a.Logger.Errorf("%q file %q stat err: %v", t.Config.Address, si.Path, err)
				continue
			}
			for _, fs := range fsi {
				a.Logger.Debugf("%q adding file %q", t.Config.Address, fs.si.Path)
				rsps = append(rsps, fs)
			}
		}
	}
	return rsps, nil
}

func (a *App) statTable(r []*fileStatResponse) string {
	targets := make([]string, 0)
	targetTabData := make(map[string][][]string)
	for _, rsps := range r {
		for _, fsi := range rsps.rsp {
			perms := os.FileMode(fsi.si.GetPermissions() & fsi.si.GetUmask()).String()
			if fsi.isDir {
				perms = "d" + perms[1:]
			}
			var lastMod string
			var size string
			if a.Config.FileStatHumanize {
				lastMod = humanize.Time(time.Unix(0, int64(fsi.si.GetLastModified())))
				size = humanize.Bytes(fsi.si.GetSize())
			} else {
				lastMod = time.Unix(0, int64(fsi.si.GetLastModified())).Format(time.RFC3339)
				size = strconv.Itoa(int((fsi.si.GetSize())))
			}
			if _, ok := targetTabData[rsps.TargetName]; !ok {
				targetTabData[rsps.TargetName] = make([][]string, 0)
				targets = append(targets, rsps.TargetName)
			}

			targetTabData[rsps.TargetName] = append(targetTabData[rsps.TargetName], []string{
				rsps.TargetName,
				fsi.si.GetPath(),
				lastMod,
				perms,
				os.FileMode(fsi.si.GetUmask()).String(),
				size,
			})
		}
	}
	// sort per target by file name
	for _, data := range targetTabData {
		sort.Slice(data, func(i, j int) bool {
			return data[i][1] < data[j][1]
		})
	}
	// sort targets
	sort.Slice(targets, func(i, j int) bool {
		return targets[i] < targets[j]
	})
	//
	b := new(bytes.Buffer)
	table := tablewriter.NewWriter(b)
	table.SetHeader([]string{"Target Name", "Path", "LastModified", "Perm", "Umask", "Size"})
	formatTable(table)
	for _, tName := range targets {
		table.AppendBulk(targetTabData[tName])
	}
	table.Render()
	return b.String()
}
