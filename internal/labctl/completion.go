package labctl

import (
	"fmt"
	"strings"

	"github.com/jurrebuunk/clusterd/internal/api"
)

const bashCompletion = `# labctl completion for bash: source <(labctl completion bash)
_labctl() {
  local cur prev cmd="" sub="" i w
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  for ((i = 1; i < COMP_CWORD; i++)); do
    w="${COMP_WORDS[i]}"
    case "$w" in
      --site) ((i++)) ;;
      -*) ;;
      *) if [ -z "$cmd" ]; then cmd="$w"; elif [ -z "$sub" ]; then sub="$w"; fi ;;
    esac
  done
  case "$prev" in
    --site) COMPREPLY=($(compgen -W "$(labctl __complete sites 2>/dev/null)" -- "$cur")); return ;;
    --host) COMPREPLY=($(compgen -W "$(labctl __complete hosts 2>/dev/null)" -- "$cur")); return ;;
    --scope) COMPREPLY=($(compgen -W "read operator manage" -- "$cur")); return ;;
    --token-file|--out) COMPREPLY=($(compgen -f -- "$cur")); return ;;
    --api|--unit|--since|--limit|--step|--time|--name) return ;;
  esac
  case "$cmd" in
    "") COMPREPLY=($(compgen -W "COMMANDS --site --json" -- "$cur")) ;;
    use) COMPREPLY=($(compgen -W "$(labctl __complete sites 2>/dev/null)" -- "$cur")) ;;
    status) [ -z "$sub" ] && COMPREPLY=($(compgen -W "host vm node ceph vip disk s3" -- "$cur")) ;;
    metrics) if [ -z "$sub" ]; then COMPREPLY=($(compgen -W "query range" -- "$cur")); else COMPREPLY=($(compgen -W "--since --step --time" -- "$cur")); fi ;;
    logs) COMPREPLY=($(compgen -W "--host --unit --since --limit --follow" -- "$cur")) ;;
    vm) if [ -z "$sub" ]; then COMPREPLY=($(compgen -W "restart stop start roll" -- "$cur")); else COMPREPLY=($(compgen -W "$(labctl __complete vms 2>/dev/null) --yes --dry-run" -- "$cur")); fi ;;
    node) if [ -z "$sub" ]; then COMPREPLY=($(compgen -W "cordon uncordon" -- "$cur")); else COMPREPLY=($(compgen -W "$(labctl __complete vms 2>/dev/null)" -- "$cur")); fi ;;
    host) if [ -z "$sub" ]; then COMPREPLY=($(compgen -W "maintenance resume" -- "$cur")); else COMPREPLY=($(compgen -W "$(labctl __complete hostnames 2>/dev/null) --reboot --yes --dry-run" -- "$cur")); fi ;;
    token) if [ -z "$sub" ]; then COMPREPLY=($(compgen -W "new" -- "$cur")); else COMPREPLY=($(compgen -W "--name --scope --out" -- "$cur")); fi ;;
    login) COMPREPLY=($(compgen -W "--api --token-file" -- "$cur")) ;;
    completion) COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")) ;;
  esac
}
complete -F _labctl labctl
`

const fishCompletion = `# labctl completion for fish: labctl completion fish | source
complete -c labctl -f
complete -c labctl -l site -r -a '(labctl __complete sites 2>/dev/null)' -d 'Site'
complete -c labctl -l json -d 'JSON output'
complete -c labctl -n __fish_use_subcommand -a 'COMMANDS'
complete -c labctl -n '__fish_seen_subcommand_from use' -a '(labctl __complete sites 2>/dev/null)'
complete -c labctl -n '__fish_seen_subcommand_from status' -a 'host vm node ceph vip disk s3'
complete -c labctl -n '__fish_seen_subcommand_from metrics; and not __fish_seen_subcommand_from query range' -a 'query range'
complete -c labctl -n '__fish_seen_subcommand_from metrics' -l since -r
complete -c labctl -n '__fish_seen_subcommand_from metrics' -l step -r
complete -c labctl -n '__fish_seen_subcommand_from metrics' -l time -r
complete -c labctl -n '__fish_seen_subcommand_from logs' -l host -r -a '(labctl __complete hosts 2>/dev/null)'
complete -c labctl -n '__fish_seen_subcommand_from logs' -l unit -r
complete -c labctl -n '__fish_seen_subcommand_from logs' -l since -r
complete -c labctl -n '__fish_seen_subcommand_from logs' -l limit -r
complete -c labctl -n '__fish_seen_subcommand_from logs' -s f -l follow
complete -c labctl -n '__fish_seen_subcommand_from vm; and not __fish_seen_subcommand_from restart stop start roll' -a 'restart stop start roll'
complete -c labctl -n '__fish_seen_subcommand_from restart stop start roll' -a '(labctl __complete vms 2>/dev/null)'
complete -c labctl -n '__fish_seen_subcommand_from restart stop start roll maintenance resume' -s y -l yes
complete -c labctl -n '__fish_seen_subcommand_from restart stop start maintenance resume' -l dry-run
complete -c labctl -n '__fish_seen_subcommand_from node; and not __fish_seen_subcommand_from cordon uncordon' -a 'cordon uncordon'
complete -c labctl -n '__fish_seen_subcommand_from cordon uncordon' -a '(labctl __complete vms 2>/dev/null)'
complete -c labctl -n '__fish_seen_subcommand_from host; and not __fish_seen_subcommand_from maintenance resume' -a 'maintenance resume'
complete -c labctl -n '__fish_seen_subcommand_from maintenance resume' -a '(labctl __complete hostnames 2>/dev/null)'
complete -c labctl -n '__fish_seen_subcommand_from maintenance' -l reboot
complete -c labctl -n '__fish_seen_subcommand_from token; and not __fish_seen_subcommand_from new' -a new
complete -c labctl -n '__fish_seen_subcommand_from new' -l name -r
complete -c labctl -n '__fish_seen_subcommand_from new' -l scope -r -a 'read operator manage'
complete -c labctl -n '__fish_seen_subcommand_from new' -l out -r -F
complete -c labctl -n '__fish_seen_subcommand_from login' -l api -r
complete -c labctl -n '__fish_seen_subcommand_from login' -l token-file -r -F
complete -c labctl -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'
`

func commandNames() string {
	var names []string
	for _, c := range commands() {
		names = append(names, c.name)
	}
	return strings.Join(names, " ")
}

func cmdCompletion(e *env, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: labctl completion bash|zsh|fish")
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(e.stdout, strings.Replace(bashCompletion, "COMMANDS", commandNames(), 1))
	case "zsh":
		// zsh runs the bash function through its bash compatibility layer.
		fmt.Fprintln(e.stdout, "# labctl completion for zsh: source <(labctl completion zsh)")
		fmt.Fprintln(e.stdout, "autoload -U +X bashcompinit && bashcompinit")
		fmt.Fprint(e.stdout, strings.Replace(bashCompletion, "COMMANDS", commandNames(), 1))
	case "fish":
		fmt.Fprint(e.stdout, strings.Replace(fishCompletion, "COMMANDS", commandNames(), 1))
	default:
		return usageErr("unknown shell %q (want bash, zsh or fish)", args[0])
	}
	return nil
}

// cmdComplete prints candidates for the completion scripts, one per line.
// It stays quiet on any error: a slow or failed lookup just completes nothing.
func cmdComplete(e *env, args []string) {
	if len(args) != 1 {
		return
	}
	switch args[0] {
	case "sites":
		for _, n := range e.cfg.siteNames() {
			fmt.Fprintln(e.stdout, n)
		}
	case "hostnames":
		c, err := e.client()
		if err != nil {
			return
		}
		var hs api.HostsStatus
		if c.Get(e.ctx, "/v1/status/hosts", nil, &hs) != nil {
			return
		}
		for _, h := range hs.Hosts {
			fmt.Fprintln(e.stdout, h.Name)
		}
	case "vms", "hosts":
		c, err := e.client()
		if err != nil {
			return
		}
		st, err := c.Status(e.ctx)
		if err != nil {
			return
		}
		if args[0] == "hosts" {
			for _, h := range st.Hosts {
				fmt.Fprintln(e.stdout, h.Name)
			}
		}
		for _, vm := range st.VMs {
			fmt.Fprintln(e.stdout, vm.Name)
		}
	}
}
