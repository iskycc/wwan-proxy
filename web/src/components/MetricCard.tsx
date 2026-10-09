import { Card, Statistic, Tooltip, Typography } from 'antd';
import type { ReactNode } from 'react';

export default function MetricCard({
  title,
  value,
  note,
  icon,
}: {
  title: string;
  value: string | number;
  note?: string;
  icon?: ReactNode;
}) {
  return (
    <Card className="metric-card">
      <Tooltip title={typeof value === 'number' ? value.toLocaleString('zh-CN') : value}>
        <Statistic
          title={
            <span>
              {icon} {title}
            </span>
          }
          value={value}
          formatter={() =>
            typeof value === 'number'
              ? new Intl.NumberFormat('zh-CN', {
                  notation: value >= 10000 ? 'compact' : 'standard',
                  maximumFractionDigits: 1,
                }).format(value)
              : value
          }
        />
      </Tooltip>
      {note && <Typography.Text type="secondary">{note}</Typography.Text>}
    </Card>
  );
}
