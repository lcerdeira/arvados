// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package install

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"git.arvados.org/arvados.git/lib/cmd"
	"git.arvados.org/arvados.git/sdk/go/ctxlog"
)

var Command cmd.Handler = installCommand{}

type installCommand struct{}

func (installCommand) RunCommand(prog string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	logger := ctxlog.New(stderr, "text", "info")
	ctx := ctxlog.Context(context.Background(), logger)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var err error
	defer func() {
		if err != nil {
			logger.WithError(err).Info("exiting")
		}
	}()

	flags := flag.NewFlagSet(prog, flag.ContinueOnError)
	flags.SetOutput(stderr)
	versionFlag := flags.Bool("version", false, "Write version information to stdout and exit 0")
	clusterType := flags.String("type", "production", "cluster `type`: development, test, or production")
	err = flags.Parse(args)
	if err == flag.ErrHelp {
		err = nil
		return 0
	} else if err != nil {
		return 2
	} else if *versionFlag {
		return cmd.Version.RunCommand(prog, args, stdin, stdout, stderr)
	}

	var dev, test, prod bool
	switch *clusterType {
	case "development":
		dev = true
	case "test":
		test = true
	case "production":
		prod = true
	default:
		err = fmt.Errorf("cluster type must be 'development', 'test', or 'production'")
		return 2
	}

	osv, err := identifyOS()
	if err != nil {
		return 1
	}

	listdir, err := os.Open("/var/lib/apt/lists")
	if err != nil {
		logger.Warnf("error while checking whether to run apt-get update: %s", err)
	} else if names, _ := listdir.Readdirnames(1); len(names) == 0 {
		// Special case for a base docker image where the
		// package cache has been deleted and all "apt-get
		// install" commands will fail unless we fetch repos.
		cmd := exec.CommandContext(ctx, "apt-get", "update")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err = cmd.Run()
		if err != nil {
			return 1
		}
	}

	if dev || test {
		debs := []string{
			"bison",
			"bsdmainutils",
			"build-essential",
			"cadaver",
			"curl",
			"cython",
			"daemontools", // lib/boot uses setuidgid to drop privileges when running as root
			"fuse",
			"gettext",
			"git",
			"gitolite3",
			"graphviz",
			"haveged",
			"iceweasel",
			"libattr1-dev",
			"libcrypt-ssleay-perl",
			"libcrypt-ssleay-perl",
			"libcurl3-gnutls",
			"libcurl4-openssl-dev",
			"libfuse-dev",
			"libgnutls28-dev",
			"libjson-perl",
			"libjson-perl",
			"libpam-dev",
			"libpcre3-dev",
			"libpq-dev",
			"libpython2.7-dev",
			"libreadline-dev",
			"libssl-dev",
			"libwww-perl",
			"libxml2-dev",
			"libxslt1.1",
			"linkchecker",
			"lsof",
			"net-tools",
			"nginx",
			"pandoc",
			"perl-modules",
			"pkg-config",
			"postgresql",
			"postgresql-contrib",
			"python",
			"python3-dev",
			"python-epydoc",
			"r-base",
			"r-cran-testthat",
			"sudo",
			"virtualenv",
			"wget",
			"xvfb",
			"zlib1g-dev",
		}
		switch {
		case osv.Debian && osv.Major >= 10:
			debs = append(debs, "libcurl4")
		default:
			debs = append(debs, "libcurl3")
		}
		cmd := exec.CommandContext(ctx, "apt-get", "install", "--yes", "--no-install-recommends")
		cmd.Args = append(cmd.Args, debs...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err = cmd.Run()
		if err != nil {
			return 1
		}
	}

	os.Mkdir("/var/lib/arvados", 0755)
	rubyversion := "2.5.7"
	if haverubyversion, err := exec.Command("/var/lib/arvados/bin/ruby", "-v").CombinedOutput(); err == nil && bytes.HasPrefix(haverubyversion, []byte("ruby "+rubyversion)) {
		logger.Print("ruby " + rubyversion + " already installed")
	} else {
		err = runBash(`
mkdir -p /var/lib/arvados/src
cd /var/lib/arvados/src
wget -c https://cache.ruby-lang.org/pub/ruby/2.5/ruby-`+rubyversion+`.tar.gz
tar xzf ruby-`+rubyversion+`.tar.gz
cd ruby-`+rubyversion+`
./configure --disable-install-doc --prefix /var/lib/arvados
make -j4
make install
/var/lib/arvados/bin/gem install bundler
cd ..
rm -r ruby-`+rubyversion+` ruby-`+rubyversion+`.tar.gz
`, stdout, stderr)
		if err != nil {
			return 1
		}
	}

	if !prod {
		goversion := "1.14"
		if havegoversion, err := exec.Command("/usr/local/bin/go", "version").CombinedOutput(); err == nil && bytes.HasPrefix(havegoversion, []byte("go version go"+goversion+" ")) {
			logger.Print("go " + goversion + " already installed")
		} else {
			err = runBash(`
cd /tmp
wget -O- https://storage.googleapis.com/golang/go`+goversion+`.linux-amd64.tar.gz | tar -C /var/lib/arvados -xzf -
ln -sf /var/lib/arvados/go/bin/* /usr/local/bin/
`, stdout, stderr)
			if err != nil {
				return 1
			}
		}

		pjsversion := "1.9.8"
		if havepjsversion, err := exec.Command("/usr/local/bin/phantomjs", "--version").CombinedOutput(); err == nil && string(havepjsversion) == "1.9.8\n" {
			logger.Print("phantomjs " + pjsversion + " already installed")
		} else {
			err = runBash(`
PJS=phantomjs-`+pjsversion+`-linux-x86_64
wget -O- https://bitbucket.org/ariya/phantomjs/downloads/$PJS.tar.bz2 | tar -C /var/lib/arvados -xjf -
ln -sf /var/lib/arvados/$PJS/bin/phantomjs /usr/local/bin/
`, stdout, stderr)
			if err != nil {
				return 1
			}
		}

		geckoversion := "0.24.0"
		if havegeckoversion, err := exec.Command("/usr/local/bin/geckodriver", "--version").CombinedOutput(); err == nil && strings.Contains(string(havegeckoversion), " "+geckoversion+" ") {
			logger.Print("geckodriver " + geckoversion + " already installed")
		} else {
			err = runBash(`
GD=v`+geckoversion+`
wget -O- https://github.com/mozilla/geckodriver/releases/download/$GD/geckodriver-$GD-linux64.tar.gz | tar -C /var/lib/arvados/bin -xzf - geckodriver
ln -sf /var/lib/arvados/bin/geckodriver /usr/local/bin/
`, stdout, stderr)
			if err != nil {
				return 1
			}
		}

		nodejsversion := "v8.15.1"
		if havenodejsversion, err := exec.Command("/usr/local/bin/node", "--version").CombinedOutput(); err == nil && string(havenodejsversion) == nodejsversion+"\n" {
			logger.Print("nodejs " + nodejsversion + " already installed")
		} else {
			err = runBash(`
NJS=`+nodejsversion+`
wget -O- https://nodejs.org/dist/${NJS}/node-${NJS}-linux-x64.tar.xz | sudo tar -C /var/lib/arvados -xJf -
ln -sf /var/lib/arvados/node-${NJS}-linux-x64/bin/{node,npm} /usr/local/bin/
`, stdout, stderr)
			if err != nil {
				return 1
			}
		}
	}

	return 0
}

type osversion struct {
	Debian bool
	Ubuntu bool
	Major  int
}

func identifyOS() (osversion, error) {
	var osv osversion
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return osv, err
	}
	defer f.Close()

	kv := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		toks := strings.SplitN(line, "=", 2)
		if len(toks) != 2 {
			return osv, fmt.Errorf("invalid line in /etc/os-release: %q", line)
		}
		k := toks[0]
		v := strings.Trim(toks[1], `"`)
		if v == toks[1] {
			v = strings.Trim(v, `'`)
		}
		kv[k] = v
	}
	if err = scanner.Err(); err != nil {
		return osv, err
	}
	switch kv["ID"] {
	case "ubuntu":
		osv.Ubuntu = true
	case "debian":
		osv.Debian = true
	default:
		return osv, fmt.Errorf("unsupported ID in /etc/os-release: %q", kv["ID"])
	}
	vstr := kv["VERSION_ID"]
	if i := strings.Index(vstr, "."); i > 0 {
		vstr = vstr[:i]
	}
	osv.Major, err = strconv.Atoi(vstr)
	if err != nil {
		return osv, fmt.Errorf("incomprehensible VERSION_ID in /etc/os/release: %q", kv["VERSION_ID"])
	}
	return osv, nil
}

func runBash(script string, stdout, stderr io.Writer) error {
	cmd := exec.Command("bash", "-")
	cmd.Stdin = bytes.NewBufferString("set -ex -o pipefail\n" + script)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
