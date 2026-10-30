import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, within } from "storybook/test";
import type { Workspace, WorkspaceStatus } from "#/api/typesGenerated";
import {
	MockHibernatedWorkspace,
	MockWorkspace,
	MockWorkspaceAgent,
	MockWorkspaceCooplay,
	MockWorkspaceResource,
} from "#/testHelpers/entities";
import { WorkspaceStatusIndicator } from "./WorkspaceStatusIndicator";

const meta: Meta<typeof WorkspaceStatusIndicator> = {
	title: "modules/workspaces/WorkspaceStatusIndicator",
	component: WorkspaceStatusIndicator,
};

export default meta;
type Story = StoryObj<typeof WorkspaceStatusIndicator>;

const createWorkspaceWithStatus = (status: WorkspaceStatus): Workspace => {
	return {
		...MockWorkspace,
		latest_build: {
			...MockWorkspace.latest_build,
			status,
		},
	} as Workspace;
};

export const Running: Story = {
	args: {
		workspace: createWorkspaceWithStatus("running"),
	},
};

export const Unhealthy: Story = {
	args: {
		workspace: {
			...createWorkspaceWithStatus("running"),
			health: {
				healthy: false,
				failing_agents: [MockWorkspaceAgent.id],
			},
			latest_build: {
				...MockWorkspace.latest_build,
				status: "running",
				resources: [
					{
						...MockWorkspaceResource,
						agents: [
							{
								...MockWorkspaceAgent,
								status: "connected",
								lifecycle_state: "start_error",
								health: { healthy: false },
							},
						],
					},
				],
			},
		},
	},
};

export const Stopped: Story = {
	args: {
		workspace: createWorkspaceWithStatus("stopped"),
	},
};

export const Starting: Story = {
	args: {
		workspace: createWorkspaceWithStatus("starting"),
	},
};

export const Stopping: Story = {
	args: {
		workspace: createWorkspaceWithStatus("stopping"),
	},
};

export const Failed: Story = {
	args: {
		workspace: createWorkspaceWithStatus("failed"),
	},
};

export const Canceling: Story = {
	args: {
		workspace: createWorkspaceWithStatus("canceling"),
	},
};

export const Canceled: Story = {
	args: {
		workspace: createWorkspaceWithStatus("canceled"),
	},
};

export const Deleting: Story = {
	args: {
		workspace: createWorkspaceWithStatus("deleting"),
	},
};

export const Deleted: Story = {
	args: {
		workspace: createWorkspaceWithStatus("deleted"),
	},
};

export const Pending: Story = {
	args: {
		workspace: createWorkspaceWithStatus("pending"),
	},
};

// A managed workspace whose microVM sleeps shows its power state, not an
// unhealthy running workspace.
export const Hibernated: Story = {
	args: { workspace: MockHibernatedWorkspace },
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText("Hibernated")).toBeVisible();
		await expect(canvas.queryByText("Running")).not.toBeInTheDocument();
	},
};

export const PowerStateUnknown: Story = {
	args: {
		workspace: {
			...MockHibernatedWorkspace,
			cooplay: {
				...MockWorkspaceCooplay,
				power: { state: "unknown", since: "2022-05-17T18:00:00Z", reason: "" },
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText("Unknown")).toBeVisible();
	},
};

export const RunningWithReducedMemory: Story = {
	args: {
		workspace: {
			...MockWorkspace,
			cooplay: {
				...MockWorkspaceCooplay,
				power: {
					state: "reduced",
					since: "2022-05-17T18:00:00Z",
					reason: "idle",
				},
			},
		},
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getByText("Running")).toBeVisible();
	},
};
