import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import { WorkspaceBuildAdmissionReasonWaitingForCapacity } from "#/api/typesGenerated";
import {
	MockCapacityWaitingWorkspace,
	MockFailedProvisionerJob,
	MockRunningProvisionerJob,
	MockWorkspaceBuildAdmission,
} from "#/testHelpers/entities";
import { WorkspaceAdmissionAlert } from "./WorkspaceAdmissionAlert";

const waitingBuild = MockCapacityWaitingWorkspace.latest_build;

const meta: Meta<typeof WorkspaceAdmissionAlert> = {
	title: "pages/WorkspacePage/WorkspaceAdmissionAlert",
	component: WorkspaceAdmissionAlert,
	args: { build: waitingBuild },
};

export default meta;
type Story = StoryObj<typeof WorkspaceAdmissionAlert>;

export const WaitingForCapacity: Story = {
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Waiting for capacity" }),
		).toBeVisible();
		await expect(
			canvas.getByText(MockWorkspaceBuildAdmission.reason),
		).toBeVisible();
		await expect(
			canvas.getByText(/position in capacity queue/i),
		).toHaveTextContent(`${MockWorkspaceBuildAdmission.queue_position}`);
	},
};

export const WaitingWithDefaultReason: Story = {
	args: {
		build: {
			...waitingBuild,
			admission: {
				...MockWorkspaceBuildAdmission,
				reason: WorkspaceBuildAdmissionReasonWaitingForCapacity,
				queue_position: undefined,
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Waiting for capacity" }),
		).toBeVisible();
		await expect(
			canvas.queryByText(WorkspaceBuildAdmissionReasonWaitingForCapacity),
		).not.toBeInTheDocument();
		await expect(
			canvas.queryByText(/position in capacity queue/i),
		).not.toBeInTheDocument();
	},
};

export const CapacityGranted: Story = {
	args: {
		build: {
			...waitingBuild,
			admission: {
				...MockWorkspaceBuildAdmission,
				status: "granted",
				reason: "",
				queue_position: undefined,
				granted_at: "2022-05-17T17:40:01.382927298Z",
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Waiting for a provisioner" }),
		).toBeVisible();
		await expect(
			canvas.queryByRole("heading", { name: "Waiting for capacity" }),
		).not.toBeInTheDocument();
	},
};

export const CapacityRejected: Story = {
	args: {
		build: {
			...waitingBuild,
			status: "failed",
			job: {
				...MockFailedProvisionerJob,
				error: "admission rejected: quota: profile large is not allowed",
			},
			admission: {
				...MockWorkspaceBuildAdmission,
				status: "rejected",
				reason: "quota: profile large is not allowed",
				queue_position: undefined,
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(
			canvas.getByRole("heading", { name: "Capacity request rejected" }),
		).toBeVisible();
		await expect(
			canvas.getByText("quota: profile large is not allowed"),
		).toBeVisible();
	},
};

export const AcquiredByProvisioner: Story = {
	args: {
		build: {
			...waitingBuild,
			status: "starting",
			job: MockRunningProvisionerJob,
			admission: {
				...MockWorkspaceBuildAdmission,
				status: "acquired",
				reason: "",
				queue_position: undefined,
			},
		},
	},
	play: async ({ canvasElement }) => {
		await expect(
			within(canvasElement).queryByRole("alert"),
		).not.toBeInTheDocument();
	},
};
