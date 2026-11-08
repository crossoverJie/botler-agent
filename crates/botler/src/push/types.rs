pub use crate::identity::ChannelSource;
pub use crate::scheduler::types::Recipient;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PushPayload {
    pub text: String,
    pub images: Vec<String>,
}
