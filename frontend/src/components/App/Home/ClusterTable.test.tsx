/*
 * Copyright 2025 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import { SnackbarProvider } from 'notistack';
import { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Cluster } from '../../../lib/k8s/cluster';
import { createMuiTheme } from '../../../lib/themes';
import ClusterContextMenu from './ClusterContextMenu';
import ClusterTable from './ClusterTable';

const theme = createMuiTheme({ name: 'light', base: 'light' });

function renderClusterTable(ui: ReactNode) {
  return render(
    <ThemeProvider theme={theme}>
      <MemoryRouter>{ui}</MemoryRouter>
    </ThemeProvider>
  );
}

function renderWithCluster(cluster: Cluster, error: any = null) {
  renderClusterTable(
    <ClusterTable
      customNameClusters={[cluster]}
      clusters={{ [cluster.name]: cluster }}
      versions={{}}
      errors={{ [cluster.name]: error }}
      warningLabels={{}}
    />
  );
}

vi.mock('react-i18next', async importOriginal => {
  const actual = await importOriginal<typeof import('react-i18next')>();
  return {
    ...actual,
    useTranslation: () => ({
      t: (key: string) => key.split('|').pop() ?? key,
    }),
  };
});

vi.mock('react-redux', () => ({
  useDispatch: () => vi.fn(),
}));

vi.mock('../../../helpers', () => ({
  default: {
    isElectron: () => true,
  },
}));

vi.mock('../../../redux/hooks', () => ({
  useTypedSelector: (selector: (state: any) => any) =>
    selector({
      clusterProvider: {
        clusterStatuses: [],
        dialogs: [],
        menuItems: [],
      },
      config: {
        allowKubeconfigChanges: true,
        isDynamicClusterEnabled: true,
      },
    }),
}));

vi.mock('../../common', () => ({
  Loader: ({ title }: { title: string }) => <div>{title}</div>,
}));

vi.mock('../../common/Table', () => ({
  default: ({ columns, data }: { columns: any[]; data: Cluster[] }) => {
    const originColumn = columns.find(column => column.id === 'origin');
    const statusColumn = columns.find(column => column.id === 'status');

    return (
      <table>
        <tbody>
          {data.map(cluster => (
            <tr
              key={cluster.name}
              data-testid={`cluster-row-${cluster.name}`}
              data-status-accessor={statusColumn.accessorFn(cluster) ?? ''}
            >
              <td>{originColumn.Cell({ row: { original: cluster } })}</td>
              <td>{statusColumn.Cell({ row: { original: cluster } })}</td>
            </tr>
          ))}
        </tbody>
      </table>
    );
  },
}));

describe('ClusterTable', () => {
  afterEach(() => {
    vi.clearAllMocks();
  });

  it('renders Cluster Inventory source labels', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_inventory',
      },
    } as Cluster);

    expect(screen.getByText('Cluster Inventory')).toBeInTheDocument();
  });

  it('renders Cluster API source labels', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_api',
      },
    } as Cluster);

    expect(screen.getByText('Cluster API')).toBeInTheDocument();
  });

  it('renders in-cluster source labels', () => {
    renderWithCluster({
      name: 'in-cluster',
      auth_type: '',
      meta_data: {
        source: 'incluster',
      },
    } as Cluster);

    expect(screen.getByText('In-cluster')).toBeInTheDocument();
  });

  it('renders unhealthy Cluster Inventory control plane status', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_inventory',
        clusterInventory: {
          conditions: [
            {
              type: 'ControlPlaneHealthy',
              status: 'False',
              reason: 'HealthCheckFailed',
              message: 'control plane endpoint is not ready',
              lastTransitionTime: '2026-05-10T00:00:00Z',
            },
          ],
        },
      },
    } as Cluster);

    expect(screen.getByText('Control plane unhealthy')).toBeInTheDocument();
  });

  it('keeps Active status for healthy Cluster Inventory clusters', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_inventory',
        clusterInventory: {
          conditions: [
            {
              type: 'ControlPlaneHealthy',
              status: 'True',
            },
          ],
        },
      },
    } as Cluster);

    expect(screen.getByText('Active')).toBeInTheDocument();
  });

  it('falls back to reachability status when Cluster Inventory condition is missing', () => {
    renderWithCluster(
      {
        name: 'spoke-a',
        auth_type: '',
        meta_data: {
          source: 'cluster_inventory',
          clusterInventory: {
            conditions: [],
          },
        },
      } as Cluster,
      { status: 500, message: 'dial tcp timeout' }
    );

    expect(screen.getByText('dial tcp timeout')).toBeInTheDocument();
  });

  it('keeps status accessor aligned with reachability errors for unknown inventory health', () => {
    renderWithCluster(
      {
        name: 'spoke-a',
        auth_type: '',
        meta_data: {
          source: 'cluster_inventory',
          clusterInventory: {
            conditions: [
              {
                type: 'ControlPlaneHealthy',
                status: 'Unknown',
              },
            ],
          },
        },
      } as Cluster,
      { status: 500, message: 'dial tcp timeout' }
    );

    expect(screen.getByText('dial tcp timeout')).toBeInTheDocument();
    expect(screen.getByTestId('cluster-row-spoke-a')).toHaveAttribute(
      'data-status-accessor',
      'dial tcp timeout'
    );
  });

  it('keeps authorization errors in the status accessor while rendering Active', () => {
    renderWithCluster(
      {
        name: 'spoke-a',
        auth_type: '',
        meta_data: {
          source: 'kubeconfig',
        },
      } as Cluster,
      { status: 403, message: 'Forbidden' }
    );

    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByTestId('cluster-row-spoke-a')).toHaveAttribute(
      'data-status-accessor',
      'Forbidden'
    );
  });

  it('renders unhealthy Cluster API status from Available condition', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_api',
        clusterAPI: {
          conditions: [
            {
              type: 'Available',
              status: 'False',
              reason: 'ControlPlaneNotReady',
              message: 'control plane endpoint is not ready',
              lastTransitionTime: '2026-05-10T00:00:00Z',
            },
          ],
        },
      },
    } as Cluster);

    expect(screen.getByText('Cluster API unhealthy')).toBeInTheDocument();
    expect(screen.getByTestId('cluster-row-spoke-a')).toHaveAttribute(
      'data-status-accessor',
      'Cluster API unhealthy'
    );
  });

  it('keeps Active status for healthy Cluster API clusters', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_api',
        clusterAPI: {
          conditions: [{ type: 'Available', status: 'True' }],
        },
      },
    } as Cluster);

    expect(screen.getByText('Active')).toBeInTheDocument();
  });

  it('falls back to reachability status when Cluster API condition is missing', () => {
    renderWithCluster(
      {
        name: 'spoke-a',
        auth_type: '',
        meta_data: {
          source: 'cluster_api',
          clusterAPI: {
            conditions: [],
          },
        },
      } as Cluster,
      { status: 500, message: 'dial tcp timeout' }
    );

    expect(screen.getByText('dial tcp timeout')).toBeInTheDocument();
  });

  it('uses Unknown accessor for unknown Cluster API condition', () => {
    renderWithCluster({
      name: 'spoke-a',
      auth_type: '',
      meta_data: {
        source: 'cluster_api',
        clusterAPI: {
          conditions: [{ type: 'Available', status: 'Unknown' }],
        },
      },
    } as Cluster);

    expect(screen.getByTestId('cluster-row-spoke-a')).toHaveAttribute(
      'data-status-accessor',
      'Unknown'
    );
  });
});

describe('ClusterContextMenu', () => {
  it('does not show delete actions for Cluster Inventory clusters', () => {
    render(
      <SnackbarProvider>
        <MemoryRouter>
          <ClusterContextMenu
            cluster={
              {
                name: 'spoke-a',
                auth_type: '',
                meta_data: {
                  source: 'cluster_inventory',
                },
              } as Cluster
            }
          />
        </MemoryRouter>
      </SnackbarProvider>
    );

    fireEvent.click(screen.getByRole('button', { name: 'Actions' }));

    expect(screen.getByText('View')).toBeInTheDocument();
    expect(screen.queryByText('Delete')).not.toBeInTheDocument();
  });
});
