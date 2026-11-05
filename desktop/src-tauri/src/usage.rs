use std::collections::HashMap;
use std::path::Path;
use std::process::Command;

#[derive(Debug, Clone, PartialEq)]
pub struct Proc {
    pub pid: u32,
    pub ppid: u32,
    pub cpu: f32,
    pub rss_kb: u64,
    pub name: String,
}

#[derive(Debug, Default, Clone, PartialEq)]
pub struct Usage {
    /// Share of the whole machine, where ps reports 100 for each full core.
    pub cpu_percent: f32,
    pub rss_kb: u64,
    pub runners: u32,
    pub jobs: u32,
}

/// Reads `ps -axo pid=,ppid=,pcpu=,rss=,comm=`. The command is last because it can hold spaces.
pub fn parse_ps(text: &str) -> Vec<Proc> {
    text.lines()
        .filter_map(|line| {
            let mut rest = line.trim_start();
            let mut field = || {
                let end = rest.find(char::is_whitespace)?;
                let (value, tail) = rest.split_at(end);
                rest = tail.trim_start();
                Some(value)
            };
            let pid = field()?.parse().ok()?;
            let ppid = field()?.parse().ok()?;
            let cpu = field()?.replace(',', ".").parse().ok()?;
            let rss_kb = field()?.parse().ok()?;
            Some(Proc { pid, ppid, cpu, rss_kb, name: rest.to_string() })
        })
        .collect()
}

/// Adds up root and everything started under it, which is the Manager, its runners and their jobs.
pub fn summarize(procs: &[Proc], root: u32, cores: usize) -> Usage {
    let mut children: HashMap<u32, Vec<&Proc>> = HashMap::new();
    for p in procs {
        children.entry(p.ppid).or_default().push(p);
    }
    let mut usage = Usage::default();
    let mut total_cpu = 0.0;
    let mut pending: Vec<&Proc> = procs.iter().filter(|p| p.pid == root).collect();
    while let Some(p) = pending.pop() {
        total_cpu += p.cpu;
        usage.rss_kb += p.rss_kb;
        match p.name.rsplit(['/', '\\']).next().unwrap_or_default() {
            "Runner.Listener" | "Runner.Listener.exe" => usage.runners += 1,
            "Runner.Worker" | "Runner.Worker.exe" => usage.jobs += 1,
            _ => {}
        }
        if let Some(list) = children.get(&p.pid) {
            pending.extend(list);
        }
    }
    usage.cpu_percent = total_cpu / cores.max(1) as f32;
    usage
}

pub fn sample(root: u32) -> Option<Usage> {
    let out = Command::new("/bin/ps").args(["-axo", "pid=,ppid=,pcpu=,rss=,comm="]).output().ok()?;
    let cores = std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1);
    Some(summarize(&parse_ps(&String::from_utf8_lossy(&out.stdout)), root, cores))
}

pub fn disk_kb(dir: &Path) -> Option<u64> {
    let out = Command::new("/usr/bin/du").arg("-sk").arg(dir).output().ok()?;
    String::from_utf8_lossy(&out.stdout).split_whitespace().next()?.parse().ok()
}

pub fn format_size(kb: u64) -> String {
    let mb = kb as f64 / 1024.0;
    if mb < 1000.0 {
        format!("{mb:.0} MB")
    } else {
        format!("{:.1} GB", mb / 1024.0)
    }
}

/// The short form for the menu bar, where a crowded bar hides whatever does not fit.
pub fn compact_size(kb: u64) -> String {
    let mb = kb as f64 / 1024.0;
    if mb < 1000.0 {
        format!("{mb:.0}M")
    } else {
        format!("{:.1}G", mb / 1024.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_ps_lines_with_spaces_and_comma_decimals() {
        let got = parse_ps("  12     1  50,5   2048 /Applications/My App/run\nnot a line\n");
        assert_eq!(got, vec![Proc { pid: 12, ppid: 1, cpu: 50.5, rss_kb: 2048, name: "/Applications/My App/run".into() }]);
    }

    #[test]
    fn counts_only_the_tree_under_the_manager() {
        let procs = parse_ps(
            "10 1 100.0 1000 /a/runner-manager\n\
             11 10 0.0 100 /bin/bash\n\
             12 11 100.0 500 /r/bin/Runner.Listener\n\
             13 12 200.0 900 /r/bin/Runner.Worker\n\
             14 13 400.0 4000 /usr/bin/xcodebuild\n\
             99 1 800.0 9999 /other/Runner.Worker\n",
        );
        let got = summarize(&procs, 10, 10);
        assert_eq!(got, Usage { cpu_percent: 80.0, rss_kb: 6500, runners: 1, jobs: 1 });
    }

    #[test]
    fn a_missing_root_counts_nothing() {
        assert_eq!(summarize(&[], 10, 4), Usage::default());
    }

    #[test]
    fn formats_sizes() {
        assert_eq!(format_size(2048), "2 MB");
        assert_eq!(format_size(3 * 1024 * 1024), "3.0 GB");
        assert_eq!(compact_size(2048), "2M");
        assert_eq!(compact_size(3 * 1024 * 1024), "3.0G");
    }
}
