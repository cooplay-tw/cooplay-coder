import { type FC, useId } from "react";
import type {
	WorkspaceCooplay,
	WorkspaceCooplayGit,
	WorkspaceCooplayJob,
	WorkspaceCooplayPR,
	WorkspaceCooplayRunState,
	WorkspaceCooplaySource,
} from "#/api/typesGenerated";
import { Badge, type BadgeProps } from "#/components/Badge/Badge";
import { Link } from "#/components/Link/Link";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "#/components/Table/Table";
import { DATE_FORMAT, formatDateTime } from "#/utils/time";

interface WorkspaceRunsPanelProps {
	cooplay: WorkspaceCooplay;
}

const runStateDisplay: Record<
	WorkspaceCooplayRunState,
	{ text: string; variant: BadgeProps["variant"] }
> = {
	running: { text: "Running", variant: "info" },
	waiting_model: { text: "Waiting for model", variant: "info" },
	waiting_human: { text: "Waiting for you", variant: "warning" },
	idle: { text: "Idle", variant: "default" },
	done: { text: "Done", variant: "green" },
	failed: { text: "Failed", variant: "destructive" },
};

const formatTime = (time: string) =>
	formatDateTime(time, DATE_FORMAT.FULL_DATETIME);

/**
 * Shows the agent Runs of a managed workspace, its latest job, the git and
 * pull request facts it reported, and which Run and grant created it, as
 * reported by the platform. Git and pull request facts come from inside the
 * workspace and are only displayed.
 */
export const WorkspaceRunsPanel: FC<WorkspaceRunsPanelProps> = ({
	cooplay,
}) => {
	const { runs, last_job, git, pr, source } = cooplay;
	const titleId = useId();
	return (
		<section
			aria-labelledby={titleId}
			className="flex flex-col gap-4 rounded-lg border border-solid border-border bg-surface-primary p-4 text-sm shadow-md"
		>
			<h2 id={titleId} className="m-0 text-base font-semibold">
				Runs
			</h2>
			{source && <CreatedBy source={source} />}
			{runs.length === 0 ? (
				<p className="m-0 text-content-secondary">
					No agent Runs in this workspace.
				</p>
			) : (
				<Table aria-label="Runs">
					<TableHeader>
						<TableRow>
							<TableHead>Run</TableHead>
							<TableHead>Harness</TableHead>
							<TableHead>State</TableHead>
							<TableHead>Started by</TableHead>
							<TableHead>Last event</TableHead>
						</TableRow>
					</TableHeader>
					<TableBody>
						{runs.map((run) => {
							const state = runStateDisplay[run.state];
							return (
								<TableRow key={run.id}>
									<TableCell className="font-mono">{run.id}</TableCell>
									<TableCell>
										{run.harness || "Unknown"}
										{run.mode && ` (mode ${run.mode})`}
									</TableCell>
									<TableCell>
										<Badge size="sm" variant={state.variant}>
											{state.text}
										</Badge>
										{run.waiting_reason && (
											<span className="ml-2">{run.waiting_reason}</span>
										)}
									</TableCell>
									<TableCell>
										{run.started_by === "grant"
											? `Grant ${run.grant_id || "unknown"}`
											: "Owner"}
									</TableCell>
									<TableCell>{formatTime(run.last_event_at)}</TableCell>
								</TableRow>
							);
						})}
					</TableBody>
				</Table>
			)}
			{(last_job || git || pr) && (
				<dl className="m-0 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2">
					{last_job && <LastJob job={last_job} />}
					{git && <GitFacts git={git} />}
					{pr && <PullRequest pr={pr} />}
				</dl>
			)}
		</section>
	);
};

const CreatedBy: FC<{ source: WorkspaceCooplaySource }> = ({ source }) => {
	if (source.kind !== "agent_grant" && !source.run_id) {
		return null;
	}
	let grant = "";
	if (source.grant_id) {
		grant = ` under grant ${source.grant_id}`;
		if (source.grant_revoked_at) {
			grant += " (revoked)";
		} else if (source.grant_expires_at) {
			grant += ` (expires ${formatTime(source.grant_expires_at)})`;
		}
	}
	return (
		<p className="m-0 text-content-secondary">
			Created by Run {source.run_id || "unknown"}
			{source.harness && ` of ${source.harness}`}
			{grant}.
		</p>
	);
};

const LastJob: FC<{ job: WorkspaceCooplayJob }> = ({ job }) => {
	let result = "Running";
	if (job.ended_at) {
		result =
			job.exit_code === undefined
				? `Ended ${formatTime(job.ended_at)}`
				: `Exited with code ${job.exit_code} at ${formatTime(job.ended_at)}`;
	}
	return (
		<>
			<dt className="text-content-secondary">Last job</dt>
			<dd className="m-0">
				<span className="block">
					{job.name || job.kind || job.id}
					{job.argv_summary && (
						<code className="ml-2 font-mono">{job.argv_summary}</code>
					)}
				</span>
				<span className="block text-content-secondary">
					Started {formatTime(job.started_at)}. {result}.
				</span>
			</dd>
		</>
	);
};

const GitFacts: FC<{ git: WorkspaceCooplayGit }> = ({ git }) => {
	const changes = [`${git.ahead} ahead`, `${git.behind} behind`];
	if (git.dirty) {
		changes.push(`${git.uncommitted_files} uncommitted files`);
	}
	if (git.merge_conflict) {
		changes.push("merge conflict");
	}
	return (
		<>
			<dt className="text-content-secondary">Git</dt>
			<dd className="m-0">
				<span className="font-mono">{git.branch || "Unknown branch"}</span>
				{git.base && ` from ${git.base}`}: {changes.join(", ")}
			</dd>
		</>
	);
};

// Pull request facts come from inside the workspace, so only web URLs
// become links.
const isWebURL = (url: string): boolean => {
	try {
		const { protocol } = new URL(url);
		return protocol === "https:" || protocol === "http:";
	} catch {
		return false;
	}
};

const PullRequest: FC<{ pr: WorkspaceCooplayPR }> = ({ pr }) => {
	const label = pr.number > 0 ? `#${pr.number}` : pr.url || "Unknown";
	return (
		<>
			<dt className="text-content-secondary">Pull request</dt>
			<dd className="m-0">
				{isWebURL(pr.url) ? (
					<Link href={pr.url} target="_blank" rel="noreferrer">
						{label}
					</Link>
				) : (
					label
				)}
				{pr.state && ` ${pr.state}`}
				{pr.ci && `, CI ${pr.ci}`}
			</dd>
		</>
	);
};
