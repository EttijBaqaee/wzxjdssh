package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version="development"

func health(c Config) error {
	host:=c.BindAddress
	if host=="" || host=="0.0.0.0" {host="127.0.0.1"}
	if host=="::" || (c.BindAddress=="" && c.BindFamily==6) {host="::1"}
	conn,err:=net.DialTimeout("tcp",net.JoinHostPort(host,fmt.Sprint(c.Port)),time.Second)
	if err!=nil {return err};defer conn.Close()
	if c.Mode=="banner" {return nil}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	line,err:=bufio.NewReader(conn).ReadString('\n')
	if err!=nil {return err}
	if !strings.HasPrefix(line,"SSH-2.0-") {return fmt.Errorf("invalid SSH identification")}
	return nil
}

func run() int {
	flags:=flag.NewFlagSet("wzxjdssh",flag.ContinueOnError)
	path:=os.Getenv("WZXJDSSH_CONFIG")
	flags.StringVar(&path,"f",path,"configuration file (required when specified)")
	flags.StringVar(&path,"config",path,"configuration file")
	var port,delay,line,max int
	var mode string
	var ipv4,ipv6,verbose,debug,showVersion,check,probe bool
	flags.IntVar(&port,"p",0,"listening port")
	flags.IntVar(&delay,"d",0,"delay in milliseconds")
	flags.IntVar(&line,"l",0,"maximum banner line length")
	flags.IntVar(&max,"m",0,"maximum admitted clients")
	flags.StringVar(&mode,"mode","","ssh or banner")
	flags.BoolVar(&ipv4,"4",false,"IPv4 only")
	flags.BoolVar(&ipv6,"6",false,"IPv6 only")
	flags.BoolVar(&verbose,"v",false,"connection logs")
	flags.BoolVar(&debug,"vv",false,"debug logs")
	flags.BoolVar(&showVersion,"V",false,"print version")
	flags.BoolVar(&check,"check",false,"validate configuration and exit")
	flags.BoolVar(&probe,"health",false,"probe configured listening socket")
	if err:=flags.Parse(os.Args[1:]);err!=nil {return 2}
	if showVersion {fmt.Println("wzxjdssh "+version);return 0}
	if flags.NArg()!=0 {fmt.Fprintln(os.Stderr,"unexpected positional arguments");return 2}
	if ipv4 && ipv6 {fmt.Fprintln(os.Stderr,"-4 and -6 are mutually exclusive");return 2}
	specified:=map[string]bool{};flags.Visit(func(f *flag.Flag){specified[f.Name]=true})
	load:=func()(Config,error){
		c,err:=LoadConfig(path);if err!=nil {return c,err}
		if serverVersion:=os.Getenv("WZXJDSSH_SERVER_VERSION");serverVersion!="" {c.ServerVersion=serverVersion}
		if specified["p"] {c.Port=port};if specified["d"] {if delay<1 || delay>86400000 {return c,fmt.Errorf("Delay must be 1..86400000 milliseconds")};c.Delay=time.Duration(delay)*time.Millisecond}
		if specified["l"] {c.MaxLineLength=line};if specified["m"] {c.MaxClients=max}
		if specified["mode"] {c.Mode=mode};if ipv4 {c.BindFamily=4};if ipv6 {c.BindFamily=6}
		if verbose {c.LogLevel=1};if debug {c.LogLevel=2}
		return c,c.Validate()
	}
	c,err:=load();if err!=nil {fmt.Fprintln(os.Stderr,"configuration:",err);return 2}
	if check {fmt.Println("configuration valid");return 0}
	if probe {if err:=health(c);err!=nil {fmt.Fprintln(os.Stderr,err);return 1};return 0}
	s,err:=NewServer(c,os.Stdout);if err!=nil {fmt.Fprintln(os.Stderr,"startup:",err);return 1}
	signals:=make(chan os.Signal,8)
	signal.Notify(signals,syscall.SIGTERM,syscall.SIGINT,syscall.SIGHUP,syscall.SIGUSR1)
	defer signal.Stop(signals)
	if err=s.Start();err!=nil {fmt.Fprintln(os.Stderr,"listen:",err);return 1}
	for sig:=range signals {
		switch sig {
		case syscall.SIGHUP:
			next,err:=load()
			if err==nil {err=s.Reload(next)}
			if err!=nil {s.log(0,"reload_rejected",map[string]any{"error":err.Error()})}
		case syscall.SIGUSR1:s.log(0,"totals",s.Snapshot())
		default:s.Stop();return 0
		}
	}
	return 0
}

func main(){os.Exit(run())}
