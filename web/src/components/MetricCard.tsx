import { Card, Statistic, Typography } from 'antd';
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
  const fullValue = typeof value === 'number' ? value.toLocaleString('zh-CN') : value;
  return (
    <Card className="metric-card">
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
        valueRender={(number) => (
          <span title={fullValue} aria-label={`${title}：${fullValue}`}>
            {number}
          </span>
        )}
      />
      {note && <Typography.Text type="secondary">{note}</Typography.Text>}
    </Card>
  );
}
