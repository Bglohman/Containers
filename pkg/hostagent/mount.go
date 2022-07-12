package hostagent

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/go-multierror"

	"github.com/lima-vm/lima/pkg/limayaml"
	"github.com/lima-vm/lima/pkg/localpathutil"
	"github.com/lima-vm/lima/pkg/osutil"
	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/sirupsen/logrus"
)

type mount struct {
	close func() error
}

func (a *HostAgent) setupMounts(ctx context.Context) ([]*mount, error) {
	var (
		res  []*mount
		mErr error
	)
	for _, f := range a.y.Mounts {
		m, err := a.setupMount(ctx, f)
		if err != nil {
			mErr = multierror.Append(mErr, err)
			continue
		}
		res = append(res, m)
	}
	return res, mErr
}

func (a *HostAgent) setupMount(ctx context.Context, m limayaml.Mount) (*mount, error) {
	location, err := localpathutil.Expand(m.Location)
	if err != nil {
		return nil, err
	}

	u, err := osutil.LimaUser(false)
	if err != nil {
		return nil, err
	}
	mountPoint, err := localpathutil.ExpandHome(m.MountPoint, u.HomeDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(location, 0755); err != nil {
		return nil, err
	}
	// NOTE: allow_other requires "user_allow_other" in /etc/fuse.conf
	sshfsOptions := "allow_other"
	if !*m.SSHFS.Cache {
		sshfsOptions = sshfsOptions + ",cache=no"
	}
	if *m.SSHFS.FollowSymlinks {
		sshfsOptions = sshfsOptions + ",follow_symlinks"
	}
	logrus.Infof("Mounting %q on %q", location, mountPoint)

	rsf := &reversesshfs.ReverseSSHFS{
		Driver:              *m.SSHFS.SFTPDriver,
		SSHConfig:           a.sshConfig,
		LocalPath:           location,
		Host:                "127.0.0.1",
		Port:                a.sshLocalPort,
		RemotePath:          mountPoint,
		Readonly:            !(*m.Writable),
		SSHFSAdditionalArgs: []string{"-o", sshfsOptions},
	}
	if err := rsf.Prepare(); err != nil {
		return nil, fmt.Errorf("failed to prepare reverse sshfs for %q on %q: %w", location, mountPoint, err)
	}
	if err := rsf.Start(); err != nil {
		logrus.WithError(err).Warnf("failed to mount reverse sshfs for %q on %q, retrying with `-o nonempty`", location, mountPoint)
		// NOTE: nonempty is not supported for libfuse3: https://github.com/canonical/multipass/issues/1381
		rsf.SSHFSAdditionalArgs = []string{"-o", "nonempty"}
		if err := rsf.Start(); err != nil {
			return nil, fmt.Errorf("failed to mount reverse sshfs for %q on %q: %w", location, mountPoint, err)
		}
	}

	res := &mount{
		close: func() error {
			logrus.Infof("Unmounting %q", location)
			if closeErr := rsf.Close(); closeErr != nil {
				return fmt.Errorf("failed to unmount reverse sshfs for %q on %q: %w", location, mountPoint, err)
			}
			return nil
		},
	}
	return res, nil
}
