import { Typography } from 'antd';

export default function DetailView({ value }: { value: unknown }) {
  return (
    <Typography.Paragraph className="detail-json">
      <pre>{JSON.stringify(value, null, 2)}</pre>
    </Typography.Paragraph>
  );
}
