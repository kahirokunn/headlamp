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

import { Icon } from '@iconify/react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import { useTheme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import {
  MRT_ColumnFiltersState,
  MRT_SortingState,
  MRT_VisibilityState,
} from 'material-react-table';
import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { generatePath, useHistory } from 'react-router-dom';
import { getClusterAppearanceFromMeta } from '../../../helpers/clusterAppearance';
import { isElectron } from '../../../helpers/isElectron';
import { loadTableSettings, storeTableSettings } from '../../../helpers/tableSettings';
import { formatClusterPathParam } from '../../../lib/cluster';
import { useClustersConf, useClustersVersion } from '../../../lib/k8s';
import { ApiError } from '../../../lib/k8s/api/v2/ApiError';
import { Cluster, KubeCondition } from '../../../lib/k8s/cluster';
import { createRouteURL } from '../../../lib/router/createRouteURL';
import { getClusterPrefixedPath } from '../../../lib/util';
import { useTypedSelector } from '../../../redux/hooks';
import { Loader } from '../../common';
import Link from '../../common/Link';
import Table from '../../common/Table';
import { LightTooltip } from '../../common/Tooltip';
import { useLocalStorageState } from '../../globalSearch/useLocalStorageState';
import ClusterBadge from '../../Sidebar/ClusterBadge';
import ClusterContextMenu from './ClusterContextMenu';
import { MULTI_HOME_ENABLED } from './config';
import { getCustomClusterNames } from './customClusterNames';

const CLUSTER_INVENTORY_SOURCE = 'cluster_inventory';
const CONTROL_PLANE_HEALTHY_CONDITION = 'ControlPlaneHealthy';
const CLUSTER_API_SOURCE = 'cluster_api';
const CLUSTER_API_STATUS_CONDITION_PRIORITY = [
  'Available',
  'Ready',
  'ControlPlaneAvailable',
  'ControlPlaneReady',
];

/**
 * Discovery condition fields used by cluster health status helpers.
 */
type ClusterDiscoveryCondition = Pick<
  KubeCondition,
  'type' | 'status' | 'reason' | 'message' | 'lastTransitionTime'
>;

/**
 * Cluster health states shown in the cluster table status column.
 */
type ClusterStatusKind = 'active' | 'error' | 'unknown';

/**
 * Status details used to render the cluster table status cell.
 */
interface ClusterStatusInfo {
  /** Visual status variant for icon and color selection. */
  kind: ClusterStatusKind;
  /** Text displayed in the status cell. */
  text: string;
  /** Discovery condition to expose in the status tooltip, when available. */
  condition: ClusterDiscoveryCondition | null;
}

const STATUS_VARIANTS: Record<
  ClusterStatusKind,
  { icon: string; colorKey: 'success' | 'error' | 'unknown'; coloredText: boolean }
> = {
  active: { icon: 'mdi:cloud-check-variant', colorKey: 'success', coloredText: true },
  error: { icon: 'mdi:cloud-off', colorKey: 'error', coloredText: true },
  unknown: { icon: 'mdi:cloud-question', colorKey: 'unknown', coloredText: false },
};

/**
 * Gets the Cluster Inventory control plane health condition for a cluster.
 */
function getControlPlaneHealthyCondition(cluster: Cluster): ClusterDiscoveryCondition | null {
  if (cluster?.meta_data?.source !== CLUSTER_INVENTORY_SOURCE) {
    return null;
  }

  const conditions = cluster?.meta_data?.clusterInventory?.conditions;
  if (!Array.isArray(conditions)) {
    return null;
  }

  return (
    conditions.find(
      (condition: ClusterDiscoveryCondition) => condition.type === CONTROL_PLANE_HEALTHY_CONDITION
    ) ?? null
  );
}

/**
 * Gets the highest-priority Cluster API status condition for a cluster.
 */
function getClusterAPIStatusCondition(cluster: Cluster): ClusterDiscoveryCondition | null {
  if (cluster?.meta_data?.source !== CLUSTER_API_SOURCE) {
    return null;
  }

  const conditions = cluster?.meta_data?.clusterAPI?.conditions;
  if (!Array.isArray(conditions)) {
    return null;
  }

  const prioritizedConditions = CLUSTER_API_STATUS_CONDITION_PRIORITY.map(conditionType =>
    conditions.find((condition: ClusterDiscoveryCondition) => condition.type === conditionType)
  ).filter(Boolean) as ClusterDiscoveryCondition[];

  return (
    prioritizedConditions.find(condition => condition.status === 'False') ??
    prioritizedConditions.find(condition => condition.status === 'Unknown') ??
    prioritizedConditions[0] ??
    null
  );
}

/**
 * Formats discovery condition fields for the status tooltip.
 */
function getConditionTooltip(condition: ClusterDiscoveryCondition): string {
  return [condition.type, condition.reason, condition.message, condition.lastTransitionTime]
    .filter(Boolean)
    .join('\n');
}

/**
 * Gets display status details for a cluster based on discovery health and reachability.
 */
function getClusterStatusInfo(
  cluster: Cluster,
  error: ApiError | null | undefined,
  t: (key: string) => string
): ClusterStatusInfo {
  const inventoryCondition = getControlPlaneHealthyCondition(cluster);

  if (inventoryCondition?.status === 'False') {
    return {
      kind: 'error',
      text: t('translation|Control plane unhealthy'),
      condition: inventoryCondition,
    };
  }

  const clusterAPICondition = getClusterAPIStatusCondition(cluster);

  if (clusterAPICondition?.status === 'False') {
    return {
      kind: 'error',
      text: t('translation|Cluster API unhealthy'),
      condition: clusterAPICondition,
    };
  }

  const condition = inventoryCondition ?? clusterAPICondition;
  const reachError = error && error.status !== 401 && error.status !== 403 ? error : null;
  if (reachError) {
    return { kind: 'error', text: reachError.message, condition };
  }

  if (condition?.status === 'Unknown' || error === undefined) {
    return { kind: 'unknown', text: '⋯', condition };
  }

  return { kind: 'active', text: t('translation|Active'), condition };
}

/**
 * Gets the sortable/filterable status value for a cluster table row.
 */
function getClusterStatusAccessor(
  cluster: Cluster,
  error: ApiError | null | undefined,
  t: (key: string) => string
): string | undefined {
  const inventoryCondition = getControlPlaneHealthyCondition(cluster);
  if (inventoryCondition?.status === 'False') {
    return t('translation|Control plane unhealthy');
  }

  const clusterAPICondition = getClusterAPIStatusCondition(cluster);
  if (clusterAPICondition?.status === 'False') {
    return t('translation|Cluster API unhealthy');
  }

  const condition = inventoryCondition ?? clusterAPICondition;
  const reachError = error && error.status !== 401 && error.status !== 403 ? error : null;
  if (reachError) {
    return reachError.message;
  }

  if (condition?.status === 'Unknown') {
    return t('translation|Unknown');
  }

  return error === null ? t('translation|Active') : error?.message;
}

/**
 * Renders the cluster status cell, including plugin-provided custom statuses.
 */
function ClusterStatus({ error, cluster }: { error?: ApiError | null; cluster: Cluster }) {
  const { t } = useTranslation(['translation']);
  const theme = useTheme();
  const customStatuses = useTypedSelector(state => state.clusterProvider.clusterStatuses);
  const renderedCustomStatus = useMemo(() => {
    for (const Status of customStatuses) {
      const renderedStatus = <Status cluster={cluster} error={error} />;
      if (renderedStatus !== null) {
        return renderedStatus;
      }
    }
    return null;
  }, [customStatuses, cluster, error]);

  if (renderedCustomStatus !== null) {
    return renderedCustomStatus;
  }

  const { kind, text, condition } = getClusterStatusInfo(cluster, error, t);
  const variant = STATUS_VARIANTS[kind];
  const color = theme.palette.home.status[variant.colorKey];
  const tooltip = condition ? getConditionTooltip(condition) : '';
  const statusContent = (
    <Box display="flex" alignItems="center" justifyContent="center" width="fit-content">
      <Icon icon={variant.icon} width={16} color={color} />
      <Typography
        variant="body2"
        style={{
          marginLeft: theme.spacing(1),
          color: variant.coloredText ? color : undefined,
        }}
      >
        {text}
      </Typography>
    </Box>
  );

  return tooltip ? (
    <LightTooltip title={<span style={{ whiteSpace: 'pre-line' }}>{tooltip}</span>}>
      {statusContent}
    </LightTooltip>
  ) : (
    statusContent
  );
}

/**
 * Props for the home cluster table.
 */
export interface ClusterTableProps {
  /** Some clusters have custom names. */
  customNameClusters: ReturnType<typeof getCustomClusterNames>;
  /** Versions for each cluster. */
  versions: ReturnType<typeof useClustersVersion>[0];
  /** Errors for each cluster. */
  errors: ReturnType<typeof useClustersVersion>[1];
  /** Clusters configuration. */
  clusters: ReturnType<typeof useClustersConf>;
  /** Warnings for each cluster. */
  warningLabels: { [cluster: string]: string };
}

const CLUSTER_TABLE_ID = 'home-clusters';

/**
 * Displays the home cluster table with cluster status, origin, warnings, and version columns.
 */
export default function ClusterTable({
  customNameClusters,
  versions,
  errors,
  clusters,
  warningLabels,
}: ClusterTableProps) {
  const history = useHistory();
  const { t } = useTranslation(['translation']);

  const [columnVisibility, setColumnVisibility] = useState<MRT_VisibilityState>(() => {
    const visibility: Record<string, boolean> = {};
    const stored = loadTableSettings(CLUSTER_TABLE_ID);
    stored.forEach(({ id, show }) => (visibility[id] = show));
    return visibility;
  });

  const [sorting, setSorting] = useLocalStorageState<MRT_SortingState>(
    `table_sorting.${CLUSTER_TABLE_ID}`,
    [{ id: 'name', desc: false }]
  );

  const [columnFilters, setColumnFilters] = useLocalStorageState<MRT_ColumnFiltersState>(
    `table_filters.${CLUSTER_TABLE_ID}`,
    []
  );

  const handleColumnVisibilityChange = useCallback(
    (updater: MRT_VisibilityState | ((old: MRT_VisibilityState) => MRT_VisibilityState)) => {
      setColumnVisibility(oldCols => {
        const newCols = typeof updater === 'function' ? updater(oldCols) : updater;
        const colsToStore = Object.entries(newCols).map(([id, show]) => ({
          id,
          show: (show ?? true) as boolean,
        }));
        storeTableSettings(CLUSTER_TABLE_ID, colsToStore);
        return newCols;
      });
    },
    []
  );

  const handleSortingChange = useCallback(
    (updater: MRT_SortingState | ((old: MRT_SortingState) => MRT_SortingState)) => {
      setSorting(old => (typeof updater === 'function' ? updater(old) : updater));
    },
    [setSorting]
  );

  const handleColumnFiltersChange = useCallback(
    (
      updater: MRT_ColumnFiltersState | ((old: MRT_ColumnFiltersState) => MRT_ColumnFiltersState)
    ) => {
      setColumnFilters(old => (typeof updater === 'function' ? updater(old) : updater));
    },
    [setColumnFilters]
  );

  /**
   * Gets the origin of a cluster.
   *
   * @param cluster
   * @returns A description of where the cluster is picked up from: dynamic, in-cluster, or from a kubeconfig file.
   */
  function getOrigin(cluster: Cluster): string {
    if (cluster?.meta_data?.source === 'kubeconfig') {
      const sourcePath = cluster?.meta_data?.origin?.kubeconfig;
      return sourcePath ? `Kubeconfig: ${sourcePath}` : 'Kubeconfig';
    } else if (cluster?.meta_data?.source === 'dynamic_cluster') {
      return t('translation|Plugin');
    } else if (cluster?.meta_data?.source === 'incluster') {
      return t('translation|In-cluster');
    } else if (cluster?.meta_data?.source === CLUSTER_INVENTORY_SOURCE) {
      return t('translation|Cluster Inventory');
    } else if (cluster?.meta_data?.source === CLUSTER_API_SOURCE) {
      return t('translation|Cluster API');
    }
    return t('translation|Unknown');
  }
  const viewClusters = t('View Clusters');

  const loading = clusters === null;
  if (loading) {
    return <Loader title={t('Loading...')} />;
  }

  const clustersList = Object.values(customNameClusters);
  if (clustersList.length === 0) {
    return (
      <Box
        display="flex"
        flexDirection="column"
        alignItems="center"
        justifyContent="center"
        minHeight="400px"
        textAlign="center"
      >
        <Icon
          icon="mdi:hexagon-multiple-outline"
          style={{ fontSize: 64, color: '#ccc', marginBottom: 16 }}
        />
        <Typography variant="h6" gutterBottom>
          {t('No clusters found')}
        </Typography>
        <Typography variant="body2" color="text.secondary" paragraph>
          {t('Add a cluster to get started.')}
        </Typography>
        {isElectron() && (
          <Button
            variant="contained"
            startIcon={<Icon icon="mdi:plus" />}
            onClick={() => {
              history.push(createRouteURL('addCluster'));
            }}
          >
            {t('Add Cluster')}
          </Button>
        )}
      </Box>
    );
  }

  return (
    <Table
      columns={[
        {
          id: 'name',
          header: t('Name'),
          accessorKey: 'name',
          gridTemplate: 2,
          Cell: ({ row: { original } }) => {
            const appearance = getClusterAppearanceFromMeta(original.name);
            return (
              <Tooltip title={original.name} arrow>
                <span>
                  <Link routeName="cluster" params={{ cluster: original.name }}>
                    <ClusterBadge
                      name={original.name}
                      icon={appearance.icon}
                      accentColor={appearance.accentColor}
                    />
                  </Link>
                </span>
              </Tooltip>
            );
          },
        },
        {
          id: 'origin',
          header: t('Origin'),
          accessorFn: cluster => getOrigin(cluster),
          Cell: ({ row: { original } }) => (
            <Typography variant="body2">{getOrigin((clusters || {})[original.name])}</Typography>
          ),
        },
        {
          id: 'status',
          header: t('Status'),
          accessorFn: cluster => getClusterStatusAccessor(cluster, errors[cluster?.name], t),
          Cell: ({ row: { original } }) => (
            <ClusterStatus error={errors[original.name]} cluster={original} />
          ),
        },
        {
          id: 'warnings',
          header: t('Warnings'),
          accessorFn: cluster => warningLabels[cluster?.name],
        },
        {
          id: 'version',
          header: t('glossary|Kubernetes Version'),
          accessorFn: ({ name }) => versions[name]?.gitVersion || '⋯',
        },
        {
          id: 'actions',
          header: t('Actions'),
          gridTemplate: 'min-content',
          muiTableBodyCellProps: {
            align: 'right',
          },
          accessorFn: cluster =>
            errors[cluster?.name] === null ? 'Active' : errors[cluster?.name]?.message,
          Cell: ({ row: { original: cluster } }) => {
            return <ClusterContextMenu cluster={cluster} />;
          },
          enableSorting: false,
          enableColumnFilter: false,
        },
      ]}
      data={clustersList}
      enableRowSelection={
        MULTI_HOME_ENABLED
          ? row => {
              // Only allow selection if the cluster is working
              return !errors[row.original.name];
            }
          : false
      }
      state={{
        columnVisibility,
        sorting,
        columnFilters,
      }}
      onColumnVisibilityChange={handleColumnVisibilityChange}
      onSortingChange={handleSortingChange}
      onColumnFiltersChange={handleColumnFiltersChange}
      muiToolbarAlertBannerProps={{
        sx: theme => ({
          background: theme.palette.background.muted,
        }),
      }}
      renderToolbarAlertBannerContent={({ table }) => (
        <Button
          variant="contained"
          sx={{
            marginLeft: 1,
          }}
          onClick={() => {
            history.push({
              pathname: generatePath(getClusterPrefixedPath(), {
                cluster: formatClusterPathParam(
                  table.getSelectedRowModel().rows.map(it => it.original.name)
                ),
              }),
            });
          }}
        >
          {viewClusters}
        </Button>
      )}
    />
  );
}
