package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"mymule/internal/api"
	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/prefs"
)

// 'mymule doctor' looks for what keeps MyMule from working well, and says
// how to fix each thing it finds.

type finding struct {
	level  int // 0 fine, 1 warning, 2 problem
	what   string
	advice string
}

type doctor struct{ list []finding }

func (d *doctor) ok(what string)                { d.list = append(d.list, finding{0, what, ""}) }
func (d *doctor) warn(what, advice string)      { d.list = append(d.list, finding{1, what, advice}) }
func (d *doctor) problem(what, advice string)   { d.list = append(d.list, finding{2, what, advice}) }
func (d *doctor) add(level int, what, a string) { d.list = append(d.list, finding{level, what, a}) }

// configErr is set when mymule.conf could not be read (doctor still runs).
var configErr error

func cmdDoctor(args []string) error {
	var d doctor
	d.checkFiles()
	if configErr == nil { // the rest needs the settings
		d.checkSettings()
		d.checkRunning()
	}
	return d.report()
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

func (d *doctor) report() error {
	marks := []string{"✓", "!", "✗"}
	warnings, problems := 0, 0
	fmt.Println("MyMule " + version + " doctor")
	for _, f := range d.list {
		fmt.Printf("  %s %s\n", marks[f.level], f.what)
		if f.advice != "" {
			for _, l := range strings.Split(f.advice, "\n") {
				fmt.Println("      → " + l)
			}
		}
		switch f.level {
		case 1:
			warnings++
		case 2:
			problems++
		}
	}
	fmt.Printf("\n%s, %s\n", plural(problems, "problem"), plural(warnings, "warning"))
	if problems > 0 {
		return errors.New("problems found")
	}
	return nil
}

// checkFiles: the data folder, mymule.conf, the download folders and the
// space left.
func (d *doctor) checkFiles() {
	dir, err := config.Dir()
	if err != nil {
		d.problem("Data folder: "+err.Error(), "Set MYMULE_HOME to a folder MyMule can write in.")
		return
	}
	if err := writable(dir); err != nil {
		d.problem("Data folder "+dir+" is not writable: "+err.Error(),
			"Give it to the user MyMule runs as, or set MYMULE_HOME to another folder.")
	} else {
		d.ok("Data folder: " + dir)
	}
	if configErr != nil {
		d.problem("mymule.conf: "+configErr.Error(), "Fix that line, or delete it to get the default back, and run mymule doctor again.")
		return
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, config.SettingsFile)); err == nil && st.Mode().Perm()&0o077 != 0 {
			d.warn(config.SettingsFile+" can be read by other users of this computer (it holds the api_key)",
				"chmod 600 "+filepath.Join(dir, config.SettingsFile))
		}
	}
	for _, f := range []struct{ name, dir string }{{"Temporary folder", settings.TempDir}, {"Download folder", settings.DownloadDir}} {
		if err := writable(f.dir); err != nil {
			d.problem(f.name+" "+f.dir+" is not writable: "+err.Error(),
				"Create it and give it to the user MyMule runs as, or change it in mymule.conf.")
			continue
		}
		free, ok := download.FreeSpace(f.dir)
		switch {
		case !ok:
			d.ok(f.name + ": " + f.dir)
		case free < 1<<30:
			d.problem(fmt.Sprintf("%s %s: only %s free", f.name, f.dir, humanSize(free)),
				"Free some space: downloads stop when there is no room for them.")
		case free < 10<<30:
			d.warn(fmt.Sprintf("%s %s: %s free", f.name, f.dir, humanSize(free)), "Big downloads may not fit.")
		default:
			d.ok(fmt.Sprintf("%s: %s (%s free)", f.name, f.dir, humanSize(free)))
		}
	}
	// The library folders and categories chosen in the web interface.
	pr := prefs.Open(filepath.Join(dir, "preferences.json"), dir)
	pr.AllowNetworkHosts(settings.NetworkHosts)
	pr.AllowRoots(append([]string{settings.DownloadDir}, settings.FolderRoots...)...)
	for _, c := range pr.Get().Categories {
		uc, ok := pr.Category(c.Name)
		if !ok {
			d.problem("Category "+c.Name+": its folder "+c.Dir+" is not accepted",
				"It must be inside MyMule's folder, download_dir or a folder listed in folder_roots in mymule.conf:\nadd its parent folder there, or choose another folder in Settings.")
			continue
		}
		if err := writable(uc.Dir); err != nil {
			d.problem("Category "+c.Name+": "+uc.Dir+" is not writable: "+err.Error(),
				"Finished downloads cannot be moved there: give it to the user MyMule runs as.")
		} else {
			d.ok("Category " + c.Name + ": " + uc.Dir)
		}
	}
	if lib := pr.Get().Library; lib.Organize {
		for _, f := range []struct{ name, dir string }{{"Movies folder", lib.MoviesDir}, {"Series folder", lib.SeriesDir}} {
			if f.dir == "" {
				continue
			}
			if err := writable(f.dir); err != nil {
				d.problem(f.name+" "+f.dir+" is not writable: "+err.Error(),
					"Finished movies and episodes cannot be moved there: give it to the user MyMule runs as.")
			} else {
				d.ok(f.name + ": " + f.dir)
			}
		}
	}
	if list, err := loadServers(); err != nil || len(list) == 0 {
		d.warn("No server list yet", "MyMule downloads one by itself when it starts; or run: mymule servers update")
	} else {
		d.ok(strconv.Itoa(len(list)) + " servers known")
	}
}

// checkSettings: what in mymule.conf is risky.
func (d *doctor) checkSettings() {
	if host, _, err := net.SplitHostPort(settings.APIListen); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			d.warn("The API and web interface ("+settings.APIListen+") can be reached from your whole network",
				"Fine on a network you trust; never forward that port in your router.")
		}
	}
	if settings.APIListen != "" && len(settings.APIKey) < 20 {
		d.warn("api_key is short", "Use a long random one: delete the api_key line and MyMule makes a new one.")
	}
	if settings.Port == 0 {
		d.warn("port = 0: nobody can connect to you (LowID)", "Set port (4662 by default) and forward it in your router.")
	}
	if !settings.Kad {
		d.warn("Kad is off", "Kad finds files and sources servers do not know: kad = true in mymule.conf.")
	}
}

// checkRunning asks the running MyMule how it is connected, or, when it is
// not running, whether its ports are free.
func (d *doctor) checkRunning() {
	c, err := daemonClient()
	if errors.Is(err, api.ErrNotRunning) {
		d.warn("MyMule is not running", "Start it with: mymule start")
		d.portFree("tcp", int(settings.Port))
		if settings.Kad {
			d.portFree("udp", int(settings.KadPort))
		}
		return
	}
	if err != nil {
		d.problem("Cannot reach the running MyMule: "+err.Error(), "Restart it: mymule stop, then mymule start.")
		return
	}
	st, err := c.Status()
	if err != nil {
		d.problem("Cannot reach the running MyMule: "+err.Error(), "Restart it: mymule stop, then mymule start.")
		return
	}
	d.ok("MyMule is running")
	fwd := fmt.Sprintf("Forward port %d (TCP) in your router to this computer", settings.Port)
	switch {
	case !st.Server.Connected:
		d.warn("Not connected to a server (yet)", "Wait a minute after starting; if it lasts, update the list: mymule servers update")
	case st.Server.HighID:
		d.ok("Server " + st.Server.Name + ": HighID")
	default:
		d.warn("Server "+st.Server.Name+": LowID, other users cannot connect to you (fewer sources)",
			fwd+", or turn on UPnP in the router (upnp = true in mymule.conf).")
	}
	if k := st.Kad; k != nil {
		switch {
		case k.Error != "":
			d.problem("Kad is not running: "+k.Error, "")
		case !k.Connected:
			d.warn("Kad: connecting", "Wait a few minutes after starting.")
		case k.Firewalled:
			d.warn("Kad: "+k.Summary(), fwd+".")
		case k.UDPFirewalled:
			d.warn("Kad: "+k.Summary(), fmt.Sprintf("Forward port %d (UDP) in your router to this computer.", settings.KadPort))
		default:
			d.ok("Kad: " + k.Summary())
		}
	}
	if r := st.Router; r != nil {
		level := 0
		if r.Method == "" && r.Error != "" && !st.Server.HighID {
			level = 1
		}
		d.add(level, "Router: "+r.Summary(), "")
	}
}

// portFree checks that nothing else uses MyMule's port.
func (d *doctor) portFree(network string, port int) {
	if port == 0 {
		return
	}
	addr := ":" + strconv.Itoa(port)
	name := fmt.Sprintf("Port %d (%s)", port, strings.ToUpper(network))
	var err error
	if network == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			l.Close()
		}
	}
	if err != nil {
		d.problem(name+" is used by another program", "Close that program (another eMule client?), or choose another port in mymule.conf.")
		return
	}
	d.ok(name + " is free")
}

// writable reports whether files can be created in dir.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".mymule-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
