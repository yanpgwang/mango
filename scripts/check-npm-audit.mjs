import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const minimumSeverity = "high";
const severityRank = new Map([
  ["info", 0],
  ["low", 1],
  ["moderate", 2],
  ["high", 3],
  ["critical", 4],
]);

// No exceptions are needed by the current Fumadocs toolchain. Any future
// exception must be justified narrowly by package and advisory URL.
const acceptedAdvisories = new Map();

const repositoryRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const result = spawnSync(
  "npm",
  ["audit", "--omit=dev", `--audit-level=${minimumSeverity}`, "--json"],
  {
    cwd: join(repositoryRoot, "website"),
    encoding: "utf8",
    maxBuffer: 32 * 1024 * 1024,
  },
);

if (result.error) {
  console.error(`npm audit could not start: ${result.error.message}`);
  process.exit(1);
}

let report;
try {
  report = JSON.parse(result.stdout);
} catch (error) {
  console.error("npm audit did not return valid JSON");
  if (result.stdout) console.error(result.stdout);
  if (result.stderr) console.error(result.stderr);
  console.error(error);
  process.exit(1);
}

if (report.error) {
  console.error("npm audit failed:", report.error);
  process.exit(1);
}

const vulnerabilities = report.vulnerabilities ?? {};
const threshold = severityRank.get(minimumSeverity);

function rootAdvisories(packageName, visiting = new Set()) {
  if (visiting.has(packageName)) return [];
  const vulnerability = vulnerabilities[packageName];
  if (!vulnerability) return [];

  const nextVisiting = new Set(visiting);
  nextVisiting.add(packageName);
  const roots = [];
  for (const cause of vulnerability.via ?? []) {
    if (typeof cause === "string") {
      const dependency = vulnerabilities[cause];
      if ((severityRank.get(dependency?.severity) ?? -1) >= threshold) {
        roots.push(...rootAdvisories(cause, nextVisiting));
      }
      continue;
    }
    if ((severityRank.get(cause.severity) ?? -1) >= threshold) {
      roots.push(cause);
    }
  }
  return roots;
}

const accepted = new Map();
const unexpected = [];
for (const [packageName, vulnerability] of Object.entries(vulnerabilities)) {
  if ((severityRank.get(vulnerability.severity) ?? -1) < threshold) continue;
  const roots = rootAdvisories(packageName);
  if (roots.length === 0) {
    unexpected.push({ packageName, title: "unresolved high-severity dependency chain" });
    continue;
  }
  for (const advisory of roots) {
    const allowedURLs = acceptedAdvisories.get(advisory.name);
    const key = `${advisory.name}:${advisory.url}`;
    if (allowedURLs?.has(advisory.url)) {
      accepted.set(key, advisory);
    } else {
      unexpected.push({ packageName, ...advisory });
    }
  }
}

if (unexpected.length > 0) {
  console.error(`npm audit found unexpected ${minimumSeverity}/critical advisories:`);
  for (const advisory of unexpected) {
    console.error(`- ${advisory.packageName}: ${advisory.title ?? advisory.url ?? "unknown advisory"}`);
  }
  process.exit(1);
}

if (accepted.size > 0) {
  console.warn("npm audit accepted unfixed upstream build-time advisories:");
  for (const advisory of accepted.values()) {
    console.warn(`- ${advisory.name}: ${advisory.url}`);
  }
}

console.log(
  `npm audit passed: ${report.metadata?.vulnerabilities?.high ?? 0} high and ` +
    `${report.metadata?.vulnerabilities?.critical ?? 0} critical dependency entries; ` +
    "all roots are explicitly accepted.",
);
