import type { FC } from "react";
import {
	type WorkspaceBuild,
	WorkspaceBuildAdmissionReasonWaitingForCapacity,
} from "#/api/typesGenerated";
import { Alert, AlertDescription, AlertTitle } from "#/components/Alert/Alert";

interface WorkspaceAdmissionAlertProps {
	build: WorkspaceBuild;
}

/**
 * Shows the capacity admission of a START build on a template managed by
 * workspace admission. A capacity wait does not occupy a provisioner, so it is
 * reported apart from the provisioner queue.
 */
export const WorkspaceAdmissionAlert: FC<WorkspaceAdmissionAlertProps> = ({
	build,
}) => {
	const { admission, job } = build;
	if (!admission) {
		return null;
	}

	if (admission.status === "rejected") {
		return (
			<Alert severity="error" prominent>
				<AlertTitle>Capacity request rejected</AlertTitle>
				<AlertDescription>
					{admission.reason || "No reason was given."}
				</AlertDescription>
			</Alert>
		);
	}

	if (job.status !== "pending") {
		return null;
	}

	if (admission.status === "granted") {
		return (
			<Alert severity="info" prominent>
				<AlertTitle>Waiting for a provisioner</AlertTitle>
				<AlertDescription>
					Capacity was granted. The build starts once a provisioner picks it up.
				</AlertDescription>
			</Alert>
		);
	}

	if (admission.status !== "waiting") {
		return null;
	}

	// The default reason only restates the title.
	const reason =
		admission.reason === WorkspaceBuildAdmissionReasonWaitingForCapacity
			? ""
			: admission.reason;
	return (
		<Alert severity="info" prominent>
			<AlertTitle>Waiting for capacity</AlertTitle>
			<AlertDescription>
				{reason && <span className="block">{reason}</span>}
				<span className="block">
					The build starts once capacity is available. It does not occupy a
					provisioner while it waits.
				</span>
				{admission.queue_position !== undefined && (
					<span className="block mt-3">
						Position in capacity queue:{" "}
						<strong>{admission.queue_position}</strong>
					</span>
				)}
			</AlertDescription>
		</Alert>
	);
};
