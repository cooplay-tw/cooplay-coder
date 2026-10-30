import type { FC, ReactNode } from "react";
import { useMutation, useQueryClient } from "react-query";
import { toast } from "sonner";
import { getErrorDetail, getErrorMessage } from "#/api/errors";
import { keepWorkspaceAwake, wakeWorkspace } from "#/api/queries/workspaces";
import type {
	Workspace,
	WorkspaceAgentWakeResult,
	WorkspaceCooplayPowerState,
} from "#/api/typesGenerated";
import { Alert, AlertDescription, AlertTitle } from "#/components/Alert/Alert";
import { Button } from "#/components/Button/Button";
import { Spinner } from "#/components/Spinner/Spinner";
import { DATE_FORMAT, formatDateTime } from "#/utils/time";
import { getWorkspaceAgents, getWorkspacePowerState } from "#/utils/workspace";

interface WorkspacePowerAlertProps {
	workspace: Workspace;
}

const asleepMessages: Partial<
	Record<WorkspaceCooplayPowerState, { title: string; detail: string }>
> = {
	paused: {
		title: "Workspace paused",
		detail: "Connecting to the workspace resumes it.",
	},
	hibernated: {
		title: "Workspace hibernated",
		detail:
			"Connecting to the workspace wakes it and restores its running processes.",
	},
	cold: {
		title: "Workspace is cold",
		detail:
			"Its processes ended. Connecting to the workspace boots it again from its disk.",
	},
	sleeping: {
		title: "Workspace is going to sleep",
		detail: "Connecting to the workspace keeps it awake.",
	},
	unknown: {
		title: "Workspace power state unknown",
		detail:
			"The platform cannot tell the state of this workspace right now. Connecting to it wakes it if it sleeps.",
	},
};

const wakeResultMessages: Record<WorkspaceAgentWakeResult, string> = {
	requested: "Waking the workspace.",
	awake: "The workspace is already awake.",
	not_managed: "The platform does not manage the power of this workspace.",
	not_started: "The workspace is not started. Start it instead.",
};

const formatTime = (time: string) =>
	formatDateTime(time, DATE_FORMAT.FULL_DATETIME);

/**
 * Shows the power state the platform reports for a managed workspace whose
 * build runs: why its agents are disconnected while its microVM sleeps,
 * with a button that wakes it, and a pending sleep with a button that keeps
 * it awake for two hours.
 */
export const WorkspacePowerAlert: FC<WorkspacePowerAlertProps> = ({
	workspace,
}) => {
	const queryClient = useQueryClient();
	const wakeMutation = useMutation(wakeWorkspace(workspace, queryClient));
	const keepAwakeMutation = useMutation(
		keepWorkspaceAwake(workspace, queryClient),
	);

	const wake = (agentId: string) => {
		wakeMutation.mutate(agentId, {
			onSuccess: (response) => {
				toast.info(wakeResultMessages[response.result]);
			},
			onError: (error) => {
				toast.error(getErrorMessage(error, "Failed to wake the workspace."), {
					description: getErrorDetail(error),
				});
			},
		});
	};
	const keepAwake = () => {
		keepAwakeMutation.mutate(undefined, {
			onSuccess: () => {
				toast.success(
					"Asked the platform to keep the workspace awake for 2 hours.",
				);
			},
			onError: (error) => {
				toast.error(
					getErrorMessage(error, "Failed to keep the workspace awake."),
					{ description: getErrorDetail(error) },
				);
			},
		});
	};

	const power = getWorkspacePowerState(workspace);
	const cooplay = workspace.cooplay;
	if (power === undefined || !cooplay?.power) {
		return null;
	}
	const { since, reason } = cooplay.power;
	const agent = getWorkspaceAgents(workspace).find((a) => a.parent_id === null);

	if (power === "waking") {
		return (
			<Alert severity="info" prominent>
				<AlertTitle>Waking the workspace</AlertTitle>
				<AlertDescription>
					Connections wait until the workspace runs again.
				</AlertDescription>
			</Alert>
		);
	}

	const asleep = asleepMessages[power];
	if (asleep) {
		const wakeButton = agent && (
			<Button
				size="sm"
				disabled={wakeMutation.isPending}
				onClick={() => wake(agent.id)}
			>
				<Spinner loading={wakeMutation.isPending} />
				Wake
			</Button>
		);
		return (
			<Alert severity="info" prominent actions={wakeButton}>
				<AlertTitle>{asleep.title}</AlertTitle>
				<AlertDescription>
					<span className="block">
						Since {formatTime(since)}
						{reason && <> ({reason})</>}.
					</span>
					<span className="block">{asleep.detail}</span>
				</AlertDescription>
			</Alert>
		);
	}

	let keepAwakeDetail: ReactNode;
	if (cooplay.keepalive_until) {
		keepAwakeDetail = (
			<span className="block">
				Kept awake until {formatTime(cooplay.keepalive_until)}.
			</span>
		);
	}

	if (cooplay.sleep_notice_at) {
		return (
			<Alert
				severity="warning"
				prominent
				actions={
					<Button
						size="sm"
						disabled={keepAwakeMutation.isPending}
						onClick={keepAwake}
					>
						<Spinner loading={keepAwakeMutation.isPending} />
						Keep awake 2 h
					</Button>
				}
			>
				<AlertTitle>Workspace goes to sleep soon</AlertTitle>
				<AlertDescription>
					<span className="block">
						The platform puts this workspace to sleep at{" "}
						{formatTime(cooplay.sleep_notice_at)}. Connecting to it later wakes
						it again.
					</span>
					{keepAwakeDetail}
				</AlertDescription>
			</Alert>
		);
	}

	if (keepAwakeDetail) {
		return (
			<Alert severity="info">
				<AlertDescription>{keepAwakeDetail}</AlertDescription>
			</Alert>
		);
	}

	return null;
};
