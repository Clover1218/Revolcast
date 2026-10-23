package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	LogDir           string         `json:"log_dir"`
	PrefixLogs       bool           `json:"prefix_logs"`
	FlushLogEachLine bool           `json:"flush_log_each_line"`
	Servers          []ServerConfig `json:"servers"`
}

type ServerConfig struct {
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	AutoStart   bool              `json:"auto_start"`
	WorkDir     string            `json:"work_dir"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env,omitempty"`
	Restart     bool              `json:"restart"`
	RestartWait int               `json:"restart_wait_seconds"`
}

type Server struct {
	cfg     ServerConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	logFile *os.File
	mu      sync.Mutex
	running bool
	started bool
	lastErr string
}

type Controller struct {
	cfg      Config
	servers  map[string]*Server
	order    []string
	current  string
	showAll  bool
	mu       sync.Mutex
	wg       sync.WaitGroup
	shutdown chan struct{}
}

func main() {
	configPath := "controller.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "controller-logs"
	}
	if err := os.MkdirAll(cfg.LogDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建日志目录失败: %v\n", err)
		os.Exit(1)
	}

	c := &Controller{
		cfg: cfg, servers: make(map[string]*Server),
		shutdown: make(chan struct{}),
	}
	for _, sc := range cfg.Servers {
		if strings.TrimSpace(sc.Name) == "" {
			fmt.Fprintln(os.Stderr, "配置错误: 服务器 name 不能为空")
			os.Exit(1)
		}
		name := strings.ToLower(sc.Name)
		if _, exists := c.servers[name]; exists {
			fmt.Fprintf(os.Stderr, "配置错误: 服务器名称重复: %s\n", sc.Name)
			os.Exit(1)
		}
		if sc.RestartWait <= 0 {
			sc.RestartWait = 3
		}
		s := &Server{cfg: sc}
		c.servers[name] = s
		c.order = append(c.order, name)
		if c.current == "" {
			c.current = name
		}
	}

	fmt.Println("Revolcast Multi-Server Controller Demo")
	fmt.Println("输入 !!help 查看命令。配置文件:", configPath)

	// Start servers sequentially to make startup output easier to read.
	for _, name := range c.order {
		s := c.servers[name]
		if s.cfg.Enabled && s.cfg.AutoStart {
			if err := c.startServer(name); err != nil {
				fmt.Printf("[controller] 启动 %s 失败: %v\n", s.cfg.Name, err)
			}
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	fmt.Printf("[controller] 输入目标: %s（输入 !!help 查看命令）\n", c.currentName())
	for {
		if !scanner.Scan() {
			break
		}
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "!!") {
			if c.handleControl(line) {
				break
			}
			continue
		}
		c.sendCurrent(line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[controller] 读取控制台输入失败: %v\n", err)
	}
	c.stopAll()
	c.wg.Wait()
	fmt.Println("[controller] 已退出")
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if len(cfg.Servers) == 0 {
		return cfg, errors.New("servers 至少需要配置一个实例")
	}
	return cfg, nil
}

func (c *Controller) prompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.current
	if s := c.servers[name]; s != nil {
		return fmt.Sprintf("[%s] > ", s.cfg.Name)
	}
	return "[no-server] > "
}

func (c *Controller) currentName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.servers[c.current]; s != nil {
		return s.cfg.Name
	}
	return "none"
}

func (c *Controller) handleControl(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "!!help":
		fmt.Println(`控制器命令:
  !!help                         显示帮助
  !!servers                      列出服务器及状态
  !!use <name>                   切换当前命令目标
  !!start <name>                 启动指定 MCDR 实例
  !!stop <name>                  停止指定 MCDR 实例
  !!restart <name>               重启指定 MCDR 实例
  !!logs <name> [n]              查看控制器保存的最近 n 行日志
  !!all                          开启所有服务器实时日志显示
  !!current                      只显示当前目标服务器的实时日志
  !!send <name> <command>        向指定服务器发送命令
  !!quit                         退出控制器并停止托管的 MCDR 进程

普通输入会原样发送给当前目标 MCDR，例如 list、!!MCDR status。
注意：!! 开头的控制器命令由本程序处理；要向 MCDR 发送同名命令，请使用 !!send <name> <command>。`)
	case "!!servers":
		c.listServers()
	case "!!use":
		if len(fields) != 2 {
			fmt.Println("用法: !!use <name>")
			break
		}
		name := strings.ToLower(fields[1])
		if c.servers[name] == nil {
			fmt.Println("未知服务器:", fields[1])
			break
		}
		c.mu.Lock()
		c.current = name
		c.showAll = false
		c.mu.Unlock()
		fmt.Printf("[controller] 输入目标已切换为 %s\n", c.servers[name].cfg.Name)
	case "!!start":
		if len(fields) != 2 {
			fmt.Println("用法: !!start <name>")
			break
		}
		if err := c.startServer(strings.ToLower(fields[1])); err != nil {
			fmt.Println("[controller] 启动失败:", err)
		}
	case "!!stop":
		if len(fields) != 2 {
			fmt.Println("用法: !!stop <name>")
			break
		}
		if err := c.stopServer(strings.ToLower(fields[1])); err != nil {
			fmt.Println("[controller] 停止失败:", err)
		}
	case "!!restart":
		if len(fields) != 2 {
			fmt.Println("用法: !!restart <name>")
			break
		}
		name := strings.ToLower(fields[1])
		if err := c.stopServer(name); err != nil {
			fmt.Println("[controller] 停止失败:", err)
			break
		}
		time.Sleep(800 * time.Millisecond)
		if err := c.startServer(name); err != nil {
			fmt.Println("[controller] 启动失败:", err)
		}
	case "!!logs":
		if len(fields) < 2 || len(fields) > 3 {
			fmt.Println("用法: !!logs <name> [n]")
			break
		}
		n := 100
		if len(fields) == 3 {
			value, err := strconv.Atoi(fields[2])
			if err != nil || value < 1 {
				fmt.Println("n 必须是正整数")
				break
			}
			n = value
		}
		if err := c.tailLogs(strings.ToLower(fields[1]), n); err != nil {
			fmt.Println("[controller] 查看日志失败:", err)
		}
	case "!!all":
		c.mu.Lock()
		c.showAll = true
		c.mu.Unlock()
		fmt.Println("[controller] 已显示所有服务器的实时输出")
	case "!!current":
		c.mu.Lock()
		c.showAll = false
		c.mu.Unlock()
		fmt.Println("[controller] 只显示当前目标服务器的实时输出")
	case "!!send":
		if len(fields) < 3 {
			fmt.Println("用法: !!send <name> <command>")
			break
		}
		name := strings.ToLower(fields[1])
		command := strings.TrimSpace(strings.TrimPrefix(line, fields[0]+" "+fields[1]))
		c.send(name, command)
	case "!!quit":
		return true
	default:
		// Unknown !! commands belong to MCDR plugins; pass them through.
		c.sendCurrent(line)
	}
	return false
}

func (c *Controller) listServers() {
	for _, name := range c.order {
		s := c.servers[name]
		s.mu.Lock()
		status := "stopped"
		if s.running {
			status = "running"
		}
		if s.lastErr != "" && !s.running {
			status = "failed: " + s.lastErr
		}
		s.mu.Unlock()
		current := ""
		if name == c.current {
			current = "  <current>"
		}
		fmt.Printf("- %-12s %-10s%s\n", s.cfg.Name, status, current)
	}
}

func (c *Controller) startServer(name string) error {
	s := c.servers[name]
	if s == nil {
		return fmt.Errorf("未知服务器: %s", name)
	}
	if !s.cfg.Enabled {
		return fmt.Errorf("%s 在配置中 enabled=false", s.cfg.Name)
	}
	if s.cfg.Command == "" {
		return fmt.Errorf("%s 未配置 command", s.cfg.Name)
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("%s 已在运行", s.cfg.Name)
	}
	s.mu.Unlock()

	if err := os.MkdirAll(c.cfg.LogDir, 0755); err != nil {
		return err
	}
	logPath := filepath.Join(c.cfg.LogDir, safeFileName(s.cfg.Name)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	if s.cfg.WorkDir != "" {
		cmd.Dir = s.cfg.WorkDir
	}
	cmd.Env = append(os.Environ(), flattenEnv(s.cfg.Env)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		logFile.Close()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logFile.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("执行 %q 失败: %w", s.cfg.Command, err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.logFile = logFile
	s.running = true
	s.started = true
	s.lastErr = ""
	s.mu.Unlock()

	fmt.Printf("[controller] 已启动 %s (pid=%d)\n", s.cfg.Name, cmd.Process.Pid)
	c.wg.Add(3)
	go c.readOutput(s, stdout, "stdout")
	go c.readOutput(s, stderr, "stderr")
	go func() {
		defer c.wg.Done()
		err := cmd.Wait()
		s.mu.Lock()
		s.running = false
		s.stdin = nil
		s.cmd = nil
		if err != nil {
			s.lastErr = err.Error()
		} else {
			s.lastErr = ""
		}
		if s.logFile != nil {
			_ = s.logFile.Close()
			s.logFile = nil
		}
		s.mu.Unlock()
		if err != nil {
			c.emit(s, fmt.Sprintf("[controller] 进程退出: %v", err))
		} else {
			c.emit(s, "[controller] 进程已退出")
		}
		if s.cfg.Restart {
			select {
			case <-c.shutdown:
				return
			case <-time.After(time.Duration(s.cfg.RestartWait) * time.Second):
			}
			select {
			case <-c.shutdown:
				return
			default:
			}
			if err := c.startServer(name); err != nil {
				c.emit(s, "[controller] 自动重启失败: "+err.Error())
			}
		}
	}()
	return nil
}

func (c *Controller) readOutput(s *Server, r io.Reader, stream string) {
	defer c.wg.Done()
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			line = stripANSI(line)
			trimmed := strings.TrimSpace(line)
			// 过滤：
			//   1) 空行
			//   2) MCDR console 的裸提示符 ">"
			//   3) MCDR 对 stdin 的回显 "   > <cmd>"（简幻欢不是 TTY，
			//      MCDR 会把用户输入又打印一遍，和简幻欢自身的输入回显叠加，
			//      看起来像“输入被重复了”）
			if trimmed == "" || trimmed == ">" || strings.HasPrefix(trimmed, "> ") {
				if err != nil {
					return
				}
				continue
			}
			if stream == "stderr" {
				line = "[stderr] " + line
			}
			c.writeLog(s, line)
			c.emit(s, line)
		}
		if err != nil {
			if err != io.EOF {
				c.emit(s, "[controller] 读取 "+stream+" 失败: "+err.Error())
			}
			return
		}
	}
}

func (c *Controller) writeLog(s *Server, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile == nil {
		return
	}
	if c.cfg.PrefixLogs {
		line = "[" + s.cfg.Name + "] " + line
	}
	_, _ = fmt.Fprintln(s.logFile, time.Now().Format("2006-01-02 15:04:05")+" "+line)
	if c.cfg.FlushLogEachLine {
		_ = s.logFile.Sync()
	}
}

func (c *Controller) emit(s *Server, line string) {
	c.mu.Lock()
	showAll := c.showAll
	current := c.current
	c.mu.Unlock()
	if showAll || strings.EqualFold(current, s.cfg.Name) {
		fmt.Printf("[%s] %s\n", s.cfg.Name, line)
	}
}

func (c *Controller) sendCurrent(line string) {
	c.mu.Lock()
	name := c.current
	c.mu.Unlock()
	c.send(name, line)
}

func (c *Controller) send(name, line string) {
	s := c.servers[name]
	if s == nil {
		fmt.Println("[controller] 未知服务器:", name)
		return
	}
	s.mu.Lock()
	stdin := s.stdin
	running := s.running
	s.mu.Unlock()
	if !running || stdin == nil {
		fmt.Printf("[controller] %s 当前未运行\n", s.cfg.Name)
		return
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(stdin, line); err != nil {
		fmt.Printf("[controller] 向 %s 写入命令失败: %v\n", s.cfg.Name, err)
	}
}

func (c *Controller) stopServer(name string) error {
	s := c.servers[name]
	if s == nil {
		return fmt.Errorf("未知服务器: %s", name)
	}
	s.mu.Lock()
	cmd := s.cmd
	stdin := s.stdin
	running := s.running
	s.mu.Unlock()
	if !running || cmd == nil {
		return fmt.Errorf("%s 当前未运行", s.cfg.Name)
	}
	// First ask MCDR to shut down gracefully. MCDR's `!!MCDR shutdown`
	// is the usual command, but the exact command can vary by MCDR version.
	if stdin != nil {
		_, _ = io.WriteString(stdin, "!!MCDR shutdown\n")
	}
	// Give MCDR a short grace period; kill only if still alive.
	go func() {
		time.Sleep(10 * time.Second)
		s.mu.Lock()
		stillRunning := s.running && s.cmd == cmd
		s.mu.Unlock()
		if stillRunning && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	return nil
}

func (c *Controller) stopAll() {
	close(c.shutdown)
	for _, name := range c.order {
		s := c.servers[name]
		s.mu.Lock()
		cmd := s.cmd
		stdin := s.stdin
		running := s.running
		s.mu.Unlock()
		if running && stdin != nil {
			_, _ = io.WriteString(stdin, "!!MCDR shutdown\n")
		}
		if running && cmd != nil {
			go func(cmd *exec.Cmd) {
				time.Sleep(10 * time.Second)
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			}(cmd)
		}
	}
}

func (c *Controller) tailLogs(name string, n int) error {
	s := c.servers[name]
	if s == nil {
		return fmt.Errorf("未知服务器: %s", name)
	}
	path := filepath.Join(c.cfg.LogDir, safeFileName(s.cfg.Name)+".log")
	lines, err := readTail(path, n)
	if err != nil {
		return err
	}
	fmt.Printf("--- %s 最近 %d 行控制器日志 ---\n", s.cfg.Name, len(lines))
	for _, line := range lines {
		fmt.Println(line)
	}
	fmt.Println("--- end ---")
	return nil
}

func readTail(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
			if len(lines) > n {
				lines = lines[1:]
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return lines, nil
}

func flattenEnv(values map[string]string) []string {
	var result []string
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(name)
}
func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				ch := s[i]
				i++
				// CSI 的终止字节是 0x40–0x7E 的字母（@A-Z[\]^_`a-z{|}~）
				if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
