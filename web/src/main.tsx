import { Component, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import { Button, Result } from 'antd';
import App from './App';
import 'antd/dist/reset.css';
import './styles.css';

class ErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    return this.state.failed ? (
      <Result
        status="error"
        title="页面加载异常"
        subTitle="请重新加载页面以恢复控制台。"
        extra={
          <Button type="primary" onClick={() => location.reload()}>
            重新加载
          </Button>
        }
      />
    ) : (
      this.props.children
    );
  }
}
createRoot(document.getElementById('root')!).render(
  <ErrorBoundary>
    <App />
  </ErrorBoundary>,
);
