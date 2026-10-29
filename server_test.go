package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestStrictConfiguration(t *testing.T) {
	defaults:=DefaultConfig()
	if defaults.Port!=2222 || defaults.Delay!=10000*time.Millisecond || defaults.MaxLineLength!=32 || defaults.MaxClients!=4096 || defaults.LogLevel!=0 || defaults.BindFamily!=0 {t.Fatalf("unexpected defaults: %+v",defaults)}
	if _,err:=parseConfig(strings.NewReader("MaxClients 2147483647\n"));err!=nil {t.Fatalf("INT_MAX rejected: %v",err)}
	if _,err:=parseConfig(strings.NewReader("MaxClients 2147483648\n"));err==nil {t.Fatal("MaxClients above INT_MAX accepted")}
	c,err:=parseConfig(strings.NewReader("Port\t2222\nDelay\t50 # tabs and comment\nServerVersion SSH-2.0-OpenSSH_9.9p2 Debian-1\n"))
	if err!=nil || c.Delay!=50*time.Millisecond {t.Fatalf("valid tab config: %+v %v",c,err)}
	for _,bad:=range []string{"Delai 1\n","BindFamily 4garbage\n","BindFamily 04\n","Delay\n","Port 0\n","Port 65536\n","Delay -1\n","Delay 9223372036854775807\n","Delay 86400001\n","Delay 50 60\n","Port 2222\nPort 2223\n","MaxClients 0\n","MaxLineLength 2\n","ServerVersion SSH-1.5-old\n","Mode fake\n","BindFamily 4\nBindAddress ::1\n"} {
		if _,err:=parseConfig(strings.NewReader(bad));err==nil {t.Errorf("accepted invalid config %q",bad)}
	}
	if _,err:=LoadConfig(filepath.Join(t.TempDir(),"absent"));err==nil {t.Fatal("explicit missing file accepted")}
}

type shortWriter struct {bytes.Buffer; width int; zero bool}
func (w *shortWriter) Write(p []byte)(int,error){if w.zero {return 0,nil};if len(p)>w.width {p=p[:w.width]};return w.Buffer.Write(p)}
func TestCompleteWrites(t *testing.T){
	w:=&shortWriter{width:2};payload:=[]byte("SSH-2.0-test\r\n")
	n,err:=writeAll(w,payload);if err!=nil || n!=len(payload) || !bytes.Equal(w.Bytes(),payload) {t.Fatalf("lost partial write %d %v %q",n,err,w.Bytes())}
	if _,err:=writeAll(&shortWriter{zero:true},payload);!errors.Is(err,io.ErrShortWrite){t.Fatal("zero-progress write must fail")}
}

func TestBannerBounds(t *testing.T){
	for _,max:=range []int{3,4,5,32,255}{for i:=0;i<500;i++{
		line,err:=randomLine(max)
		if err!=nil || len(line)<3 || len(line)>max || !bytes.HasSuffix(line,[]byte("\r\n")) || bytes.HasPrefix(line,[]byte("SSH-")){t.Fatalf("invalid banner %q %v",line,err)}
		for _,b:=range line[:len(line)-2]{if b<32 || b>126 {t.Fatal("invalid ASCII")}}
	}}
}

func freePort(t *testing.T)int{t.Helper();l,err:=net.Listen("tcp4","127.0.0.1:0");if err!=nil {t.Fatal(err)};defer l.Close();return l.Addr().(*net.TCPAddr).Port}
var suiteKeyDirectory string

func TestMain(m *testing.M){
	dir,err:=os.MkdirTemp("","wzxjdssh-test-keys-");if err!=nil {panic(err)}
	suiteKeyDirectory=dir
	_,err=loadHostKeys(dir);if err!=nil {panic(err)}
	code:=m.Run();os.RemoveAll(dir);os.Exit(code)
}

func testConfig(t *testing.T)Config{
	t.Helper();c:=DefaultConfig();c.Port=freePort(t);c.BindAddress="127.0.0.1";c.BindFamily=4;c.HostKeyDirectory=suiteKeyDirectory;c.Delay=40*time.Millisecond;c.IdleTimeout=time.Second;c.HandshakeTimeout=time.Second;c.MaxConnectionTime=5*time.Second;c.MaxClientsPerIP=0;c.ConnectionsPerMinute=0;return c
}
func startTest(t *testing.T,c Config)*Server{t.Helper();s,err:=NewServer(c,nil);if err!=nil {t.Fatal(err)};if err=s.Start();err!=nil {t.Fatal(err)};t.Cleanup(s.Stop);return s}
func address(c Config)string{return net.JoinHostPort("127.0.0.1",fmt.Sprint(c.Port))}
func connect(t *testing.T,c Config)net.Conn{t.Helper();conn,err:=net.DialTimeout("tcp4",address(c),time.Second);if err!=nil {t.Fatal(err)};return conn}
func bannerLine(t *testing.T,conn net.Conn)string{t.Helper();conn.SetReadDeadline(time.Now().Add(2*time.Second));line,err:=bufio.NewReader(conn).ReadString('\n');if err!=nil {t.Fatal(err)};return line}
func eventually(t *testing.T,fn func()bool){t.Helper();end:=time.Now().Add(2*time.Second);for time.Now().Before(end){if fn(){return};time.Sleep(5*time.Millisecond)};t.Fatal("condition not reached")}

func TestStableHostKeys(t *testing.T){
	a,err:=loadHostKeys(suiteKeyDirectory);if err!=nil {t.Fatal(err)};b,err:=loadHostKeys(suiteKeyDirectory);if err!=nil {t.Fatal(err)}
	if len(a)!=3 {t.Fatal("expected Ed25519, ECDSA and RSA")}
	for i:=range a {if !bytes.Equal(a[i].PublicKey().Marshal(),b[i].PublicKey().Marshal()){t.Fatal("host key changed")}}
}

func TestRealSSHTransportAndDeniedAuthentication(t *testing.T){
	c:=testConfig(t);s:=startTest(t,c)
	_,key,_:=ed25519.GenerateKey(rand.Reader);signer,_:=ssh.NewSignerFromKey(key)
	cases:=[]struct{name string; auth ssh.AuthMethod}{
		{"password",ssh.Password("TEST-SECRET-MUST-NOT-APPEAR")},
		{"publickey",ssh.PublicKeys(signer)},
		{"keyboard-interactive",ssh.KeyboardInteractive(func(_, _ string,questions []string,echo []bool)([]string,error){if len(questions)!=1 || echo[0]{return nil,errors.New("wrong prompt")};return []string{"TEST-SECRET-MUST-NOT-APPEAR"},nil})},
	}
	for _,tc:=range cases {t.Run(tc.name,func(t *testing.T){
		seen:=false;started:=time.Now()
		client:=&ssh.ClientConfig{User:"probe",Auth:[]ssh.AuthMethod{tc.auth},Timeout:2*time.Second,HostKeyCallback:func(host string,remote net.Addr,key ssh.PublicKey)error{seen=true;return nil}}
		conn,err:=ssh.Dial("tcp",address(c),client);if conn!=nil {conn.Close()}
		if err==nil || !seen {t.Fatalf("transport did not complete or auth unexpectedly accepted: %v %v",seen,err)}
		if time.Since(started)<c.Delay {t.Fatal("authentication was not delayed")}
	})}
	eventually(t,func()bool{return s.stats.active.Load()==0})
	if s.stats.authAttempts.Load()<3 {t.Fatal("authentication not counted")}
}

func TestSSHIdentification(t *testing.T){
	c:=testConfig(t);startTest(t,c);conn:=connect(t,c);defer conn.Close()
	if got:=bannerLine(t,conn);got!=c.ServerVersion+"\r\n" {t.Fatalf("wrong identification %q",got)}
}

func TestReloadRollbackAndLiveMigration(t *testing.T){
	c:=testConfig(t);c.Mode="banner";s:=startTest(t,c);existing:=connect(t,c);defer existing.Close();bannerLine(t,existing)
	occupied,err:=net.Listen("tcp4","127.0.0.1:0");if err!=nil {t.Fatal(err)};defer occupied.Close()
	next:=c;next.Port=occupied.Addr().(*net.TCPAddr).Port
	if err=s.Reload(next);err==nil {t.Fatal("accepted occupied port")}
	if s.cfg.Load().Port!=c.Port {t.Fatal("configuration changed after failed rebind")}
	fresh:=connect(t,c);bannerLine(t,fresh);fresh.Close();bannerLine(t,existing)
	next.Port=freePort(t)
	if err=s.Reload(next);err!=nil {t.Fatal(err)}
	probe:=connect(t,next);bannerLine(t,probe);probe.Close();bannerLine(t,existing)
	if old,err:=net.DialTimeout("tcp4",address(c),100*time.Millisecond);err==nil {old.Close();t.Fatal("old listener still open")}
	invalid:=next;invalid.MaxClients=0
	if err=s.Reload(invalid);err==nil || s.cfg.Load().MaxClients!=next.MaxClients {t.Fatal("invalid config not rolled back")}
}

func TestReloadShorterDelayNoFIFO(t *testing.T){
	c:=testConfig(t);c.Mode="banner";c.Delay=time.Second;s:=startTest(t,c)
	first:=connect(t,c);defer first.Close();eventually(t,func()bool{return s.stats.active.Load()==1})
	time.Sleep(60*time.Millisecond);next:=c;next.Delay=30*time.Millisecond
	if err:=s.Reload(next);err!=nil {t.Fatal(err)}
	started:=time.Now();second:=connect(t,next);defer second.Close();bannerLine(t,second)
	if elapsed:=time.Since(started);elapsed>400*time.Millisecond {t.Fatalf("new connection blocked %v",elapsed)}
	started=time.Now();bannerLine(t,first);if time.Since(started)>400*time.Millisecond {t.Fatal("existing timer was not refreshed")}
}

func TestMonotonicTimersAndCancellation(t *testing.T){
	c:=testConfig(t);c.Delay=time.Second;s:=startTest(t,c);ctx,cancel:=context.WithCancel(context.Background());cancel()
	if err:=s.pause(ctx);!errors.Is(err,context.Canceled){t.Fatal(err)}
	started:=time.Now();ctx,cancel=context.WithTimeout(context.Background(),30*time.Millisecond);defer cancel();err:=s.pause(ctx)
	if err==nil || time.Since(started)>500*time.Millisecond {t.Fatal("timer deadline not enforced")}
	// time.Time carries monotonic data; these deadlines never compare wall epochs.
	if started==started.Round(0) {t.Fatal("monotonic component missing")}
}

func TestAdmissionLimitsAndCounters(t *testing.T){
	c:=testConfig(t);c.Mode="banner";c.Delay=time.Second;c.MaxClients=1;s:=startTest(t,c)
	first:=connect(t,c);eventually(t,func()bool{return s.stats.active.Load()==1})
	second:=connect(t,c);second.SetReadDeadline(time.Now().Add(time.Second));_,err:=second.Read(make([]byte,1));second.Close();if err==nil {t.Fatal("over-limit connection served")}
	if s.stats.accepted.Load()!=1 || s.stats.rejected.Load()!=1 {t.Fatalf("incorrect counters: %v",s.Snapshot())}
	first.Close();eventually(t,func()bool{return s.stats.active.Load()==0})
	third:=connect(t,c);third.Close();eventually(t,func()bool{return s.stats.accepted.Load()==2})
}

func TestPerIPRateAndHandshakeLimits(t *testing.T){
	for _,kind:=range []string{"per_ip","rate","handshake"}{t.Run(kind,func(t *testing.T){
		c:=testConfig(t)
		switch kind{case "per_ip":c.MaxClientsPerIP=1;case "rate":c.ConnectionsPerMinute=1;case "handshake":c.MaxPendingHandshakes=1}
		s:=startTest(t,c);first:=connect(t,c);defer first.Close();bannerLine(t,first)
		eventually(t,func()bool{return s.stats.active.Load()==1})
		second:=connect(t,c);second.SetReadDeadline(time.Now().Add(time.Second));_,err:=second.Read(make([]byte,1));second.Close()
		if err==nil {t.Fatal("limit bypassed")}
		eventually(t,func()bool{return s.stats.rejected.Load()==1})
	})}
}

func TestHandshakeAndLifetimeTimeout(t *testing.T){
	c:=testConfig(t);c.HandshakeTimeout=100*time.Millisecond;s:=startTest(t,c)
	conn:=connect(t,c);bannerLine(t,conn);conn.SetReadDeadline(time.Now().Add(time.Second));_,err:=conn.Read(make([]byte,1));conn.Close()
	if err==nil {t.Fatal("idle handshake not closed")};eventually(t,func()bool{return s.stats.active.Load()==0 && s.stats.pending.Load()==0})
	bc:=testConfig(t);bc.Mode="banner";bc.Delay=time.Second;bc.MaxConnectionTime=100*time.Millisecond;bs:=startTest(t,bc)
	b:=connect(t,bc);b.SetReadDeadline(time.Now().Add(time.Second));_,err=b.Read(make([]byte,1));b.Close();if err==nil {t.Fatal("lifetime not enforced")};eventually(t,func()bool{return bs.stats.active.Load()==0})
}

func TestServerDelayDoesNotTriggerPeerIdleTimeout(t *testing.T){
	c:=testConfig(t);c.IdleTimeout=80*time.Millisecond;c.Delay=220*time.Millisecond;startTest(t,c)
	started:=time.Now();conn,err:=ssh.Dial("tcp",address(c),&ssh.ClientConfig{User:"probe",Auth:[]ssh.AuthMethod{ssh.Password("ignored")},HostKeyCallback:ssh.InsecureIgnoreHostKey(),Timeout:time.Second})
	if conn!=nil {conn.Close()}
	if err==nil || !strings.Contains(err.Error(),"unable to authenticate") || time.Since(started)<c.Delay {t.Fatalf("server delay counted as idle: %v",err)}
}

func TestConcurrentClientsAndReload(t *testing.T){
	c:=testConfig(t);c.Mode="banner";c.Delay=10*time.Millisecond;s:=startTest(t,c)
	var wg sync.WaitGroup;fail:=make(chan error,64)
	for i:=0;i<32;i++ {wg.Add(1);go func(){defer wg.Done();conn,err:=net.DialTimeout("tcp",address(c),time.Second);if err!=nil {fail<-err;return};defer conn.Close();conn.SetReadDeadline(time.Now().Add(2*time.Second));_,err=bufio.NewReader(conn).ReadString('\n');if err!=nil {fail<-err}}()}
	for i:=0;i<10;i++ {next:=c;next.Delay=time.Duration(10+i)*time.Millisecond;if err:=s.Reload(next);err!=nil {t.Fatal(err)}}
	wg.Wait();close(fail);for err:=range fail {t.Error(err)}
	eventually(t,func()bool{return s.stats.active.Load()==0})
	if s.stats.accepted.Load()!=32 {t.Fatalf("accepted %d",s.stats.accepted.Load())}
}

func TestStopInterruptsAuthenticationDelay(t *testing.T){
	c:=testConfig(t);c.Delay=time.Hour;c.MaxConnectionTime=time.Hour;s:=startTest(t,c)
	done:=make(chan struct{})
	go func(){defer close(done);conn,_:=ssh.Dial("tcp",address(c),&ssh.ClientConfig{User:"probe",Auth:[]ssh.AuthMethod{ssh.Password("ignored")},HostKeyCallback:ssh.InsecureIgnoreHostKey(),Timeout:time.Second});if conn!=nil {conn.Close()}}()
	eventually(t,func()bool{return s.stats.active.Load()==1 && s.stats.pending.Load()==0})
	start:=time.Now();s.Stop();if time.Since(start)>time.Second {t.Fatal("shutdown blocked by delay")}
	select{case <-done:case <-time.After(time.Second):t.Fatal("client not closed")}
}

type synchronizedBuffer struct{mu sync.Mutex;b bytes.Buffer}
func (b *synchronizedBuffer) Write(p []byte)(int,error){b.mu.Lock();defer b.mu.Unlock();return b.b.Write(p)}
func (b *synchronizedBuffer) String()string{b.mu.Lock();defer b.mu.Unlock();return b.b.String()}

func TestSecretsNotLogged(t *testing.T){
	c:=testConfig(t);c.LogLevel=2;log:=&synchronizedBuffer{};s,err:=NewServer(c,log);if err!=nil {t.Fatal(err)};if err=s.Start();err!=nil {t.Fatal(err)};defer s.Stop()
	client,_:=ssh.Dial("tcp",address(c),&ssh.ClientConfig{User:"UNLOGGED-USER",Auth:[]ssh.AuthMethod{ssh.Password("UNLOGGED-PASSWORD")},HostKeyCallback:ssh.InsecureIgnoreHostKey(),Timeout:time.Second});if client!=nil {client.Close()}
	eventually(t,func()bool{return s.stats.active.Load()==0})
	output:=log.String();if strings.Contains(output,"UNLOGGED-PASSWORD") || strings.Contains(output,"UNLOGGED-USER"){t.Fatal("credential leaked into logs")}
	if !strings.Contains(output,"authentication"){t.Fatal("authentication event missing")}
}

func TestOpenSSHClientCompatibility(t *testing.T){
	if _,err:=exec.LookPath("ssh");err!=nil {t.Fatal("OpenSSH test client missing")}
	c:=testConfig(t);startTest(t,c)
	keyfile:=filepath.Join(t.TempDir(),"known_hosts")
	cmd:=exec.Command("ssh","-vv","-p",fmt.Sprint(c.Port),"-o","StrictHostKeyChecking=no","-o","UserKnownHostsFile="+keyfile,"-o","BatchMode=yes","-o","PreferredAuthentications=none","-o","ConnectTimeout=2","probe@127.0.0.1","true")
	out,err:=cmd.CombinedOutput();text:=string(out)
	if err==nil || !strings.Contains(text,"Remote protocol version 2.0") || !strings.Contains(text,"kex: algorithm:") || !strings.Contains(text,"Permission denied"){t.Fatalf("OpenSSH interop failed: %v\n%s",err,text)}
	if strings.Contains(text,"no matching") {t.Fatal(text)}
	t.Log("OpenSSH completed identification, key exchange, host signature and authentication negotiation")
}

func TestHealthProbe(t *testing.T){for _,mode:=range []string{"ssh","banner"}{c:=testConfig(t);c.Mode=mode;startTest(t,c);if err:=health(c);err!=nil {t.Fatalf("%s health: %v",mode,err)}}}

func TestProcessSignalsAndCLI(t *testing.T){
	dir:=t.TempDir();binary:=os.Getenv("WZX_TEST_BINARY")
	if binary=="" {binary=filepath.Join(dir,"wzxjdssh");if out,err:=exec.Command("go","build","-o",binary,".").CombinedOutput();err!=nil {t.Fatalf("process fixture build: %v %s",err,out)}}
	missing:=exec.Command(binary,"-f",filepath.Join(dir,"absent"),"-check");if err:=missing.Run();err==nil {t.Fatal("CLI accepted missing config")}
	conf:=filepath.Join(dir,"config");c:=testConfig(t)
	writeConf:=func(port int,extra string){t.Helper();text:=fmt.Sprintf("Port %d\nBindFamily 4\nBindAddress 127.0.0.1\nHostKeyDirectory %s\nDelay 50\nLogLevel 1\n%s",port,suiteKeyDirectory,extra);if err:=os.WriteFile(conf,[]byte(text),0600);err!=nil {t.Fatal(err)}}
	writeConf(c.Port,"")
	check:=exec.Command(binary,"-f",conf,"-check");if out,err:=check.CombinedOutput();err!=nil {t.Fatalf("check %v %s",err,out)}
	log:=&synchronizedBuffer{};cmd:=exec.Command(binary,"-f",conf);cmd.Stdout=log;cmd.Stderr=log
	if err:=cmd.Start();err!=nil {t.Fatal(err)}
	done:=make(chan error,1);go func(){done<-cmd.Wait()}()
	defer func(){cmd.Process.Kill()}()
	eventually(t,func()bool{return strings.Contains(log.String(),"listening")})
	cmd.Process.Signal(syscall.SIGUSR1);eventually(t,func()bool{return strings.Contains(log.String(),"totals")})
	occupied,err:=net.Listen("tcp4","127.0.0.1:0");if err!=nil {t.Fatal(err)};defer occupied.Close()
	writeConf(occupied.Addr().(*net.TCPAddr).Port,"");cmd.Process.Signal(syscall.SIGHUP)
	eventually(t,func()bool{return strings.Contains(log.String(),"reload_rejected")})
	conn:=connect(t,c);if !strings.HasPrefix(bannerLine(t,conn),"SSH-2.0-"){t.Fatal("old listener lost")};conn.Close()
	nextPort:=freePort(t);writeConf(nextPort,"");cmd.Process.Signal(syscall.SIGHUP)
	eventually(t,func()bool{return strings.Contains(log.String(),"reloaded")})
	c.Port=nextPort;conn=connect(t,c);bannerLine(t,conn);conn.Close()
	cmd.Process.Signal(syscall.SIGTERM)
	select {case err:=<-done:if err!=nil {t.Fatalf("ungraceful exit %v %s",err,log.String())};case <-time.After(3*time.Second):t.Fatal("SIGTERM timeout")}
	if !strings.Contains(log.String(),"closed") {t.Fatal("missing closure logs")}
}

func TestProcessEnvironmentServerVersion(t *testing.T){
	binary:=os.Getenv("WZX_TEST_BINARY")
	if binary=="" {binary=filepath.Join(t.TempDir(),"wzxjdssh");if out,err:=exec.Command("go","build","-o",binary,".").CombinedOutput();err!=nil {t.Fatalf("process fixture build: %v %s",err,out)}}
	for _,tc:=range []struct{name,env,fileVersion,want string}{
		{"default","","",DefaultConfig().ServerVersion},
		{"custom","SSH-2.0-OpenSSH_9.9","","SSH-2.0-OpenSSH_9.9"},
		{"custom_with_comment","SSH-2.0-OpenSSH_9.9 Custom-1","","SSH-2.0-OpenSSH_9.9 Custom-1"},
		{"empty_uses_config","","SSH-2.0-Configured_1","SSH-2.0-Configured_1"},
		{"environment_overrides_config","SSH-2.0-Environment_2","SSH-2.0-Configured_1","SSH-2.0-Environment_2"},
	}{t.Run(tc.name,func(t *testing.T){
		c:=testConfig(t);path:=filepath.Join(t.TempDir(),"config")
		text:=fmt.Sprintf("Port %d\nBindFamily 4\nBindAddress 127.0.0.1\nHostKeyDirectory %s\nLogLevel 1\n",c.Port,suiteKeyDirectory)
		if tc.fileVersion!="" {text+="ServerVersion "+tc.fileVersion+"\n"}
		if err:=os.WriteFile(path,[]byte(text),0600);err!=nil {t.Fatal(err)}
		log:=&synchronizedBuffer{};cmd:=exec.Command(binary,"-f",path);cmd.Env=append(os.Environ(),"WZXJDSSH_SERVER_VERSION="+tc.env);cmd.Stdout=log;cmd.Stderr=log
		if err:=cmd.Start();err!=nil {t.Fatal(err)}
		defer func(){cmd.Process.Kill();cmd.Wait()}()
		eventually(t,func()bool{return strings.Contains(log.String(),"listening")})
		conn:=connect(t,c);defer conn.Close()
		if got:=bannerLine(t,conn);got!=tc.want+"\r\n" {t.Fatalf("environment identification %q, want %q",got,tc.want)}
	})}
	for _,invalid:=range []string{"SSH-2.0-test\r\ninjected","SSH-1.5-old","SSH-2.0-","SSH-2.0-测试"}{
		cmd:=exec.Command(binary,"-check");cmd.Env=append(os.Environ(),"WZXJDSSH_CONFIG=","WZXJDSSH_SERVER_VERSION="+invalid)
		if out,err:=cmd.CombinedOutput();err==nil {t.Fatalf("invalid environment accepted: %q %s",invalid,out)}
	}
}

func TestProductionImageSSH(t *testing.T){
	target:=os.Getenv("WZX_TEST_TARGET")
	if target=="" {t.Skip("production image target only available in Compose")}
	expectedVersion:=os.Getenv("WZX_TEST_EXPECT_VERSION")
	if expectedVersion=="" {expectedVersion="SSH-2.0-OpenSSH_10.0p2 Debian-7+deb13u4"}
	var fingerprints []string
	for i:=0;i<2;i++{
		var peerVersion string
		var fingerprint string
		config:=&ssh.ClientConfig{User:"image-probe",ClientVersion:"SSH-2.0-OpenSSH_9.9p2",Timeout:3*time.Second,HostKeyCallback:func(_ string,_ net.Addr,key ssh.PublicKey)error{fingerprint=ssh.FingerprintSHA256(key);return nil}}
		raw,err:=net.DialTimeout("tcp",target,3*time.Second);if err!=nil {t.Fatal(err)}
		// Read server identification ourselves, then replay it to the SSH client.
		raw.SetReadDeadline(time.Now().Add(3*time.Second));reader:=bufio.NewReader(raw);ident,err:=reader.ReadString('\n');if err!=nil {raw.Close();t.Fatal(err)};peerVersion=strings.TrimSpace(ident)
		wrapped:=&replayConn{Conn:raw,reader:io.MultiReader(strings.NewReader(ident),reader)}
		conn,_,_,authErr:=ssh.NewClientConn(wrapped,target,config);raw.Close();if conn!=nil {conn.Close()}
		if authErr==nil || fingerprint=="" || peerVersion!=expectedVersion {t.Fatalf("invalid production handshake: %q %q %v",peerVersion,fingerprint,authErr)}
		fingerprints=append(fingerprints,fingerprint)
	}
	if fingerprints[0]!=fingerprints[1] {t.Fatal("production host key changed across connections")}
	file:="/results/production-host-key.json"
	if prior,err:=os.ReadFile(file);err==nil {
		var previous map[string]any
		if json.Unmarshal(prior,&previous)==nil && previous["fingerprint"]!=fingerprints[0]{t.Fatal("production host key changed across container restart")}
	}
	data,_:=json.MarshalIndent(map[string]any{"target":target,"fingerprint":fingerprints[0],"server_version":expectedVersion,"transport_verified":true,"authentication_accepted":false},"","  ")
	if err:=os.WriteFile(file,data,0600);err!=nil {t.Fatal(err)}
	t.Log("production scratch image completed real key exchange, stable host signature and auth negotiation")
}

type replayConn struct{net.Conn;reader io.Reader}
func (r *replayConn) Read(p []byte)(int,error){return r.reader.Read(p)}
